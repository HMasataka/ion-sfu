# Receiverアーキテクチャの詳細解説

## Receiverの役割

Receiverは、ion-sfuにおいて**着信RTPストリーム（Publisherからのメディア）を受信し、複数のDownTrack（Subscriberへのストリーム）に配信する**コアコンポーネントです。

## 1. Receiverの生成数

### 基本原則

- **Receiverはトラック（trackID）ごとに1つ作成されます**
- クライアント（ピア）の数ではなく、**メディアトラックの数に依存**します
- 例：
  - 1つのPublisherが音声トラックとビデオトラックを送信する場合 → **2つのReceiver**が作成されます
  - 3つのPublisherがそれぞれ1つのビデオトラックを送信する場合 → **3つのReceiver**が作成されます

### 生成タイミングとコード

Receiverは`router.go:128-233`の`AddReceiver`メソッドで生成されます：

```go
func (r *router) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote,
    trackID, streamID string) (Receiver, bool) {

    // trackIDごとに一度だけReceiverを作成
    recv, ok := r.receivers[trackID]
    if !ok {
        // 新しいReceiverを作成
        recv = NewWebRTCReceiver(receiver, track, r.id)
        r.receivers[trackID] = recv
        publish = true
    }

    // Simulcastの場合、同じReceiverに複数のレイヤーを追加
    recv.AddUpTrack(track, buff, r.config.Simulcast.BestQualityFirst)

    return recv, publish
}
```

### Simulcastの特殊ケース

Simulcast（同時に複数の品質のストリームを送信）の場合：

- **1つのReceiverが最大3つのレイヤー（品質）を管理**します
- 各レイヤーは別々の`TrackRemote`と`Buffer`を持ちます
- レイヤー構成（`receiver.go:144-152`）：
  - レイヤー0: 低品質（quarter resolution, RID=""）
  - レイヤー1: 中品質（half resolution, RID="h"）
  - レイヤー2: 高品質（full resolution, RID="f"）

## 2. Receiverの主要な役割

### 2.1 RTPパケットの受信とバッファリング

- Bufferからパケットを読み取り（`receiver.go:377`）
- ExtendedPacket形式で受信（タイムスタンプ、キーフレーム情報を含む）

### 2.2 DownTrackへの配信

**これがReceiverの最も重要な役割です：**

```go
// receiver.go:364-411
func (w *WebRTCReceiver) writeRTP(layer int) {
    for {
        // Bufferからパケット読み取り
        pkt, err := w.buffers[layer].ReadExtended()

        // このレイヤーをサブスクライブしているすべてのDownTrackに配信
        for _, dt := range w.downTracks[layer].Load().([]*DownTrack) {
            if err = dt.WriteRTP(pkt, layer); err != nil {
                // エラー処理
            }
        }
    }
}
```

### 2.3 DownTrackの管理

- **AddDownTrack**（`receiver.go:196-230`）: 新しいSubscriberにDownTrackを追加
- **DeleteDownTrack**（`receiver.go:271-292`）: Subscriberが切断時にDownTrackを削除
- **SwitchDownTrack**（`receiver.go:232-244`）: Simulcastレイヤー切り替え時のDownTrack移動

### 2.4 Simulcastレイヤー管理

- レイヤーごとに独立したDownTrack配列を管理（`receiver.go:89`）
- キーフレーム待機による滑らかなレイヤー切り替え（`receiver.go:383-398`）

### 2.5 NACK処理（パケット再送）

- DownTrackからのNACK要求を処理（`receiver.go:314-362`）
- Worker Poolで効率的に再送処理

## 3. Sessionへの書き込み

### Receiverは直接Sessionに書き込みません

**重要なポイント：ReceiverはSessionオブジェクトに直接書き込むことはありません。**

実際の流れは以下の通りです：

### 3.1 Publisher → Router → Session

```
Publisher (OnTrack)
  ↓
Router.AddReceiver() (router.go:128)
  ↓
Session.Publish(router, receiver) (session.go:283)
```

**Publisherのコード**（`publisher.go:106-135`）：

```go
pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
    // RouterがReceiverを作成
    r, pub := p.router.AddReceiver(receiver, track, track.ID(), track.StreamID())

    if pub {
        // SessionにReceiverを公開（他のSubscriberに配信）
        p.session.Publish(p.router, r)
    }
})
```

### 3.2 SessionのPublishメソッド

**Sessionは既存のすべてのSubscriberにReceiverを配信します**（`session.go:283-297`）：

```go
func (s *SessionLocal) Publish(router Router, r Receiver) {
    for _, p := range s.Peers() {
        // 自分自身にはサブスクライブしない
        if router.ID() == p.ID() || p.Subscriber() == nil {
            continue
        }

        // RouterがSubscriberにDownTrackを追加
        if err := router.AddDownTracks(p.Subscriber(), r); err != nil {
            Logger.Error(err, "Error subscribing transport to Router")
        }
    }
}
```

## 4. RouterとDownTrackとの関係

### 4.1 Router（ルーター）

**Routerの役割：**

- 各Publisherに1つのRouterが存在（`publisher.go:102`）
- **Receiverの生成と管理**
- **ReceiverからSubscriberへのDownTrackの作成**
- RTCP処理の集約

### 4.2 メディア配信フロー

```
Publisher (WebRTC PeerConnection)
    ↓ RTP packets
[Buffer] ← RTPReceiver (Pion WebRTC)
    ↓ read packets
[Receiver] (1つのトラックに対して1つ)
    ↓ write to all
[DownTrack 1] [DownTrack 2] [DownTrack N] (Subscriberの数だけ)
    ↓           ↓           ↓
Subscriber1  Subscriber2  SubscriberN
```

### 4.3 Router.AddDownTrack（`router.go:268-319`）

Routerが**ReceiverとDownTrackを繋ぐ**中核機能：

```go
func (r *router) AddDownTrack(sub *Subscriber, recv Receiver) (*DownTrack, error) {
    // 1. DownTrackを作成
    downTrack, err := NewDownTrack(codec, recv, r.bufferFactory, sub.id, maxPacketTrack)

    // 2. WebRTC Transceiverを追加
    downTrack.transceiver, err = sub.pc.AddTransceiverFromTrack(downTrack, ...)

    // 3. SubscriberにDownTrackを登録
    sub.AddDownTrack(recv.StreamID(), downTrack)

    // 4. ReceiverにDownTrackを登録（ここで接続完了）
    recv.AddDownTrack(downTrack, r.config.Simulcast.BestQualityFirst)

    return downTrack, nil
}
```

### 4.4 DownTrack（ダウントラック）

**DownTrackの役割：**

- **1つのSubscriberへの1つのトラック送信を管理**
- Receiverから受け取ったパケットをSubscriberに送信
- シーケンス番号とタイムスタンプの調整
- Simulcastレイヤー切り替え処理
- 適応的ビットレート制御（パケットロスに基づく）

**DownTrackの生成数：**

- **各SubscriberのトラックごとにDownTrackが1つ作成されます**
- 例：3人のSubscriberが1つのビデオトラックを受信 → **3つのDownTrack**

## 5. データフロー全体像

### 5.1 初期接続フロー

```
1. Publisher.pc.OnTrack() トリガー
   ↓
2. Router.AddReceiver()
   - Receiverを作成（trackIDごとに1つ）
   - Bufferを作成してトラックにBind
   ↓
3. Session.Publish()
   - 既存のすべてのSubscriberに通知
   ↓
4. Router.AddDownTrack() （各Subscriberに対して）
   - DownTrackを作成
   - Receiver.AddDownTrack() を呼び出し
   ↓
5. Receiver.writeRTP() ゴルーチン開始
   - 各レイヤーごとに1つのゴルーチン
```

### 5.2 実行時のパケットフロー

```
WebRTC Track (Publisher)
    ↓
Buffer.ReadExtended() ← [Receiver.writeRTP() goroutine]
    ↓ loop over downTracks[layer]
DownTrack.WriteRTP() ← for each subscriber
    ↓
Subscriber's WebRTC PeerConnection
```

### 5.3 Simulcastの場合

```
Publisher Simulcast Track (3 layers)
    ↓ RID="", "h", "f"
[Receiver]
  ├─ Layer 0 (low)    → DownTrack配列 → Subscribers
  ├─ Layer 1 (medium) → DownTrack配列 → Subscribers
  └─ Layer 2 (high)   → DownTrack配列 → Subscribers

各SubscriberのDownTrackは動的にレイヤーを切り替え可能
```

## 6. 重要なコード参照

### 6.1 Receiver作成

- `router.go:128-233` - AddReceiver()
- `receiver.go:98-109` - NewWebRTCReceiver()

### 6.2 DownTrack管理

- `receiver.go:196-230` - AddDownTrack()
- `receiver.go:364-411` - writeRTP() (配信ループ)

### 6.3 Session統合

- `publisher.go:106-135` - OnTrack handler
- `session.go:283-297` - Publish()
- `router.go:268-319` - AddDownTrack()

### 6.4 Simulcast処理

- `receiver.go:139-194` - AddUpTrack()
- `receiver.go:232-244` - SwitchDownTrack()
- `downtrack.go:416-516` - writeSimulcastRTP()

## まとめ

1. **Receiverの生成数**: トラックIDごとに1つ（クライアント数ではない）
2. **Sessionへの書き込み**: ReceiverはSessionに直接書き込まず、`Session.Publish()`がRouterとReceiverを仲介
3. **Routerとの関係**: Routerが**ReceiverとDownTrackを接続**し、メディアルーティングを管理
4. **DownTrackとの関係**: **1つのReceiverから複数のDownTrack**に配信（1対多の関係）
5. **データフロー**: Publisher → Buffer → Receiver → DownTracks → Subscribers
