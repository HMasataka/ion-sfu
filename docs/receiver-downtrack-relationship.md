# ReceiverとDownTrackの関係の詳細解説

## 概要

**ReceiverとDownTrackは、ion-sfuにおけるメディアストリーム配信の核となる1対多の関係です。**

```
1つのReceiver (着信メディア)
    ↓ 配信
複数のDownTrack (各Subscriberへの送信メディア)
```

## 1. 基本的な関係性

### 1.1 オーナーシップと責任

```
Receiver (受信側)
├── 役割: Publisherから着信したRTPストリームを管理
├── 責任:
│   ├── Bufferからのパケット読み取り
│   ├── すべてのDownTrackへの配信
│   └── DownTrackの登録・削除管理
└── データ構造: downTracks [3]atomic.Value ([]*DownTrack)

DownTrack (送信側)
├── 役割: 1つのSubscriberへのRTPストリーム送信を管理
├── 責任:
│   ├── パケットの変換（SSRC、シーケンス番号、タイムスタンプ）
│   ├── Simulcastレイヤー切り替え
│   ├── 適応的ビットレート制御
│   └── NACK処理とパケット再送要求
└── データ構造: receiver Receiver (参照のみ保持)
```

### 1.2 生成数の関係

**重要な比率：**

- **1つのReceiver : N個のDownTrack**（N = Subscriberの数）
- 例：
  - 1つのPublisherが1つのビデオトラックを送信
  - 3つのSubscriberが受信する場合
  - → **1つのReceiver、3つのDownTrack**

## 2. 接続の確立

### 2.1 接続フロー全体

```
1. Router.AddDownTrack() (router.go:268)
   - DownTrackオブジェクトを作成
   - Receiverへの参照を渡す
   ↓
2. DownTrack作成 (downtrack.go:102-112)
   - ReceiverへのポインタをDownTrack内に保存
   - d.receiver = r
   ↓
3. Receiver.AddDownTrack() (receiver.go:196-230)
   - DownTrackをReceiverのリストに登録
   - レイヤー（Simulcast）を決定
   - DownTrackの初期設定
   ↓
4. 接続完了
   - 双方向の関係が確立
   - Receiver → DownTrack (配信用)
   - DownTrack → Receiver (RTCP、再送要求用)
```

### 2.2 Router.AddDownTrack()の詳細

```go
// router.go:268-319
func (r *router) AddDownTrack(sub *Subscriber, recv Receiver) (*DownTrack, error) {
    // 1. 重複チェック（既に接続されているか）
    for _, dt := range sub.GetDownTracks(recv.StreamID()) {
        if dt.ID() == recv.TrackID() {
            return dt, nil
        }
    }

    // 2. DownTrackを作成（Receiverへの参照を渡す）
    downTrack, err := NewDownTrack(codec, recv, bufferFactory, sub.id, maxPacketTrack)

    // 3. WebRTC Transceiverを追加
    downTrack.transceiver, err = sub.pc.AddTransceiverFromTrack(downTrack, ...)

    // 4. SubscriberにDownTrackを登録
    sub.AddDownTrack(recv.StreamID(), downTrack)

    // 5. ReceiverにDownTrackを登録（接続完了）
    recv.AddDownTrack(downTrack, r.config.Simulcast.BestQualityFirst)

    return downTrack, nil
}
```

### 2.3 DownTrack作成時の接続

```go
// downtrack.go:102-112
func NewDownTrack(c webrtc.RTPCodecCapability, r Receiver, bf *buffer.Factory,
    peerID string, mt int) (*DownTrack, error) {

    return &DownTrack{
        id:            r.TrackID(),     // ReceiverからtrackIDをコピー
        streamID:      r.StreamID(),    // ReceiverからstreamIDをコピー
        receiver:      r,               // Receiverへの参照を保存★
        codec:         c,
        bufferFactory: bf,
        peerID:        peerID,
        maxTrack:      mt,
    }, nil
}
```

### 2.4 Receiver.AddDownTrack()の詳細

```go
// receiver.go:196-230
func (w *WebRTCReceiver) AddDownTrack(track *DownTrack, bestQualityFirst bool) {
    if w.closed.get() {
        return
    }

    // レイヤーを決定
    layer := 0
    if w.isSimulcast {
        // Simulcastの場合、最高品質または最低品質から開始
        for i, t := range w.available {
            if t.get() {
                layer = i
                if !bestQualityFirst {
                    break  // 最低品質で停止
                }
            }
        }

        // DownTrackにSimulcast設定を適用
        track.SetInitialLayers(int32(layer), 2)
        track.maxSpatialLayer = 2
        track.maxTemporalLayer = 2
        track.lastSSRC = w.SSRC(layer)
        track.trackType = SimulcastDownTrack
        track.payload = packetFactory.Get().(*[]byte)
    } else {
        // Simpleトラックの場合
        track.SetInitialLayers(0, 0)
        track.trackType = SimpleDownTrack
    }

    // DownTrackをリストに追加
    w.Lock()
    w.storeDownTrack(layer, track)
    w.Unlock()
}
```

## 3. データフロー：Receiver → DownTrack

### 3.1 RTPパケット配信（主要フロー）

**Receiverがすべての接続されたDownTrackにパケットを配信します：**

```go
// receiver.go:364-411
func (w *WebRTCReceiver) writeRTP(layer int) {
    for {
        // 1. Bufferからパケットを読み取り
        pkt, err := w.buffers[layer].ReadExtended()
        if err == io.EOF {
            return
        }

        // 2. Simulcastレイヤー切り替え処理
        if w.isSimulcast {
            if w.pending[layer].get() {
                if pkt.KeyFrame {
                    // キーフレーム到着時にレイヤー切り替えを完了
                    w.Lock()
                    for idx, dt := range w.pendingTracks[layer] {
                        w.deleteDownTrack(dt.CurrentSpatialLayer(), dt.id)
                        w.storeDownTrack(layer, dt)
                        dt.SwitchSpatialLayerDone(int32(layer))
                        w.pendingTracks[layer][idx] = nil
                    }
                    w.pendingTracks[layer] = w.pendingTracks[layer][:0]
                    w.pending[layer].set(false)
                    w.Unlock()
                } else {
                    // キーフレームを要求
                    w.SendRTCP(pli)
                }
            }
        }

        // 3. このレイヤーの全DownTrackに配信★
        for _, dt := range w.downTracks[layer].Load().([]*DownTrack) {
            if err = dt.WriteRTP(pkt, layer); err != nil {
                if err == io.EOF || err == io.ErrClosedPipe {
                    // エラー時はDownTrackを削除
                    w.Lock()
                    w.deleteDownTrack(layer, dt.id)
                    w.Unlock()
                }
                Logger.Error(err, "Error writing to down track", "id", dt.id)
            }
        }
    }
}
```

**重要なポイント：**

- Receiverは各レイヤーごとに`writeRTP()`ゴルーチンを実行
- レイヤーごとに独立したDownTrackリストを保持
- すべてのDownTrackに同じパケットを配信（ファンアウト）

### 3.2 DownTrack.WriteRTP()の処理

```go
// downtrack.go:187-199
func (d *DownTrack) WriteRTP(p *buffer.ExtPacket, layer int) error {
    if !d.enabled.get() || !d.bound.get() {
        return nil
    }

    switch d.trackType {
    case SimpleDownTrack:
        return d.writeSimpleRTP(p)
    case SimulcastDownTrack:
        return d.writeSimulcastRTP(p, layer)
    }
    return nil
}
```

### 3.3 データフロー図

```
[Buffer Layer 0 (Low)]
    ↓ ReadExtended()
[Receiver.writeRTP(0)] ← goroutine
    ↓ for each DownTrack
    ├─→ DownTrack A.WriteRTP() → Subscriber A
    ├─→ DownTrack B.WriteRTP() → Subscriber B
    └─→ DownTrack C.WriteRTP() → Subscriber C

[Buffer Layer 1 (Mid)]
    ↓ ReadExtended()
[Receiver.writeRTP(1)] ← goroutine
    ↓ for each DownTrack
    ├─→ DownTrack D.WriteRTP() → Subscriber D
    └─→ DownTrack E.WriteRTP() → Subscriber E

[Buffer Layer 2 (High)]
    ↓ ReadExtended()
[Receiver.writeRTP(2)] ← goroutine
    ↓ for each DownTrack
    └─→ DownTrack F.WriteRTP() → Subscriber F
```

## 4. 逆方向フロー：DownTrack → Receiver

### 4.1 RTCP処理（フィードバック）

**DownTrackはSubscriberからのRTCPを受信し、Receiverに転送します：**

```go
// downtrack.go:518-586
func (d *DownTrack) handleRTCP(bytes []byte) {
    pkts, err := rtcp.Unmarshal(bytes)

    var fwdPkts []rtcp.Packet

    for _, pkt := range pkts {
        switch p := pkt.(type) {
        case *rtcp.PictureLossIndication:
            // PLI（キーフレーム要求）をReceiverに転送
            p.MediaSSRC = ssrc
            p.SenderSSRC = d.ssrc
            fwdPkts = append(fwdPkts, p)

        case *rtcp.TransportLayerNack:
            // NACK（パケット再送要求）
            if d.sequencer != nil {
                var nackedPackets []packetMeta
                for _, pair := range p.Nacks {
                    nackedPackets = append(nackedPackets,
                        d.sequencer.getSeqNoPairs(pair.PacketList())...)
                }
                // Receiverにパケット再送を要求★
                if err = d.receiver.RetransmitPackets(d, nackedPackets); err != nil {
                    return
                }
            }
        }
    }

    // PLI/FIRをReceiverに転送★
    if len(fwdPkts) > 0 {
        d.receiver.SendRTCP(fwdPkts)
    }
}
```

### 4.2 パケット再送処理

**ReceiverがBufferから過去のパケットを取得し、DownTrackに再送します：**

```go
// receiver.go:314-362
func (w *WebRTCReceiver) RetransmitPackets(track *DownTrack, packets []packetMeta) error {
    if w.nackWorker.Stopped() {
        return io.ErrClosedPipe
    }

    w.nackWorker.Submit(func() {
        src := packetFactory.Get().(*[]byte)
        for _, meta := range packets {
            pktBuff := *src
            buff := w.buffers[meta.layer]
            if buff == nil {
                break
            }

            // 1. Bufferから過去のパケットを取得
            i, err := buff.GetPacket(pktBuff, meta.sourceSeqNo)
            if err != nil {
                continue
            }

            // 2. パケットをアンマーシャル
            var pkt rtp.Packet
            if err = pkt.Unmarshal(pktBuff[:i]); err != nil {
                continue
            }

            // 3. DownTrack用にヘッダーを書き換え
            pkt.Header.SequenceNumber = meta.targetSeqNo
            pkt.Header.Timestamp = meta.timestamp
            pkt.Header.SSRC = track.ssrc
            pkt.Header.PayloadType = track.payloadType

            // 4. DownTrackに再送★
            if _, err = track.writeStream.WriteRTP(&pkt.Header, pkt.Payload); err != nil {
                Logger.Error(err, "Writing rtx packet err")
            } else {
                track.UpdateStats(uint32(i))
            }
        }
        packetFactory.Put(src)
    })
    return nil
}
```

### 4.3 逆方向フロー図

```
Subscriber (WebRTC client)
    ↓ RTCP packets (PLI, NACK, REMB, RR)
[DownTrack.handleRTCP()]
    ↓ PLI/FIR
    ├─→ d.receiver.SendRTCP(fwdPkts) → Router.rtcpCh → Publisher
    ↓ NACK
    └─→ d.receiver.RetransmitPackets(d, packets)
         ↓
    [Receiver.RetransmitPackets()]
         ↓ Buffer.GetPacket()
    [Buffer] ← 過去のパケット取得
         ↓
    [DownTrack.writeStream.WriteRTP()] → Subscriber
```

## 5. Simulcastにおける特殊な関係

### 5.1 レイヤーごとのDownTrack管理

**Receiverは3つのレイヤーを管理し、各レイヤーごとに独立したDownTrackリストを保持：**

```go
// receiver.go:70-95
type WebRTCReceiver struct {
    // ...
    buffers        [3]*buffer.Buffer      // レイヤーごとのBuffer
    upTracks       [3]*webrtc.TrackRemote // レイヤーごとの着信トラック
    downTracks     [3]atomic.Value        // レイヤーごとのDownTrackリスト★
    available      [3]atomicBool          // レイヤーが利用可能か
    pending        [3]atomicBool          // レイヤー切り替え待機中か
    pendingTracks  [3][]*DownTrack        // レイヤー切り替え待機中のDownTrack
    // ...
}
```

### 5.2 レイヤー切り替えフロー

```
1. DownTrackがレイヤー切り替えを要求
   ↓
   d.receiver.SwitchDownTrack(d, targetLayer)

2. Receiverがペンディングリストに追加 (receiver.go:232-244)
   ↓
   w.pending[layer].set(true)
   w.pendingTracks[layer] = append(w.pendingTracks[layer], track)

3. Receiverがキーフレームを待機 (receiver.go:383-398)
   ↓
   if pkt.KeyFrame {
       // キーフレーム到着時に切り替え実行
   }

4. DownTrackを古いレイヤーから削除し、新しいレイヤーに追加
   ↓
   w.deleteDownTrack(dt.CurrentSpatialLayer(), dt.id)
   w.storeDownTrack(layer, dt)
   dt.SwitchSpatialLayerDone(int32(layer))
```

### 5.3 Simulcast配信の例

```
Publisher (Simulcast 3 layers)
    ↓ Layer 0, 1, 2
[Receiver]
    ├── Layer 0 (Low)  → [DownTrack配列]
    │   ├── DownTrack A (Subscriber A - 低品質希望)
    │   └── DownTrack B (Subscriber B - 一時的に低品質)
    │
    ├── Layer 1 (Mid)  → [DownTrack配列]
    │   └── DownTrack C (Subscriber C)
    │
    └── Layer 2 (High) → [DownTrack配列]
        ├── DownTrack D (Subscriber D)
        └── DownTrack E (Subscriber E)

各DownTrackは動的にレイヤー間を移動可能
（パケットロス、帯域幅に基づいて自動調整）
```

## 6. ライフサイクル管理

### 6.1 作成

```
1. Router.AddDownTrack() が呼ばれる
2. NewDownTrack() でDownTrackオブジェクト作成
   - Receiverへの参照を保存
3. Receiver.AddDownTrack() でReceiverに登録
   - DownTrackをレイヤーリストに追加
```

### 6.2 実行中

```
Receiver側:
- writeRTP() ゴルーチンでパケット配信
- RetransmitPackets() でパケット再送
- SwitchDownTrack() でレイヤー切り替え

DownTrack側:
- WriteRTP() でパケット受信と変換
- handleRTCP() でRTCPフィードバック処理
- handleLayerChange() で適応的レイヤー調整
```

### 6.3 削除

```
1. DownTrack.Close() が呼ばれる
   ↓
2. DownTrack.OnCloseHandler() 実行 (router.go:298-310)
   - sub.pc.RemoveTrack()
   - sub.RemoveDownTrack()
   - sub.negotiate()
   ↓
3. Receiver.DeleteDownTrack() (receiver.go:271-292)
   - w.deleteDownTrack(layer, id)
   - dt.Close()
   ↓
4. DownTrackがリストから削除され、接続解除
```

## 7. 双方向インターフェース

### 7.1 Receiver → DownTrack

**Receiverが提供するDownTrack向けのインターフェース：**

```go
// receiver.go:48-67
type Receiver interface {
    // RTP配信（実装：Receiver.writeRTP()内でdt.WriteRTP()を呼び出し）
    // DownTrack管理
    AddDownTrack(track *DownTrack, bestQualityFirst bool)
    DeleteDownTrack(layer int, id string)
    SwitchDownTrack(track *DownTrack, layer int) error

    // DownTrackからの要求処理
    RetransmitPackets(track *DownTrack, packets []packetMeta) error
    SendRTCP(p []rtcp.Packet)

    // 情報取得
    GetBitrate() [3]uint64
    GetMaxTemporalLayer() [3]int32
    GetSenderReportTime(layer int) (rtpTS uint32, ntpTS uint64)

    // メタデータ
    TrackID() string
    StreamID() string
    Codec() webrtc.RTPCodecParameters
}
```

### 7.2 DownTrack → Receiver

**DownTrackが使用するReceiverのメソッド：**

```go
// DownTrack内での使用例

// 1. RTCP転送 (downtrack.go:584)
d.receiver.SendRTCP(fwdPkts)

// 2. パケット再送要求 (downtrack.go:573)
d.receiver.RetransmitPackets(d, nackedPackets)

// 3. Sender Report情報取得 (downtrack.go:348)
srRTP, srNTP := d.receiver.GetSenderReportTime(layer)

// 4. Simulcastレイヤー切り替え (downtrack.go:246)
d.receiver.SwitchDownTrack(d, targetLayer)
```

## 8. データ構造の詳細

### 8.1 Receiver内のDownTrack管理

```go
// receiver.go:89-91
type WebRTCReceiver struct {
    // ...
    downTracks     [3]atomic.Value // 各要素は[]*DownTrack
    pending        [3]atomicBool
    pendingTracks  [3][]*DownTrack
    // ...
}

// DownTrackの追加
func (w *WebRTCReceiver) storeDownTrack(layer int, dt *DownTrack) {
    dts := w.downTracks[layer].Load().([]*DownTrack)
    ndts := make([]*DownTrack, len(dts)+1)
    copy(ndts, dts)
    ndts[len(ndts)-1] = dt
    w.downTracks[layer].Store(ndts)  // atomic操作
}

// DownTrackの削除
func (w *WebRTCReceiver) deleteDownTrack(layer int, id string) {
    dts := w.downTracks[layer].Load().([]*DownTrack)
    ndts := make([]*DownTrack, 0, len(dts))
    for _, dt := range dts {
        if dt.id != id {
            ndts = append(ndts, dt)
        } else {
            dt.Close()
        }
    }
    w.downTracks[layer].Store(ndts)  // atomic操作
}
```

**atomic.Valueを使用する理由：**

- `writeRTP()`ゴルーチンが同時にリストを読み取る
- DownTrackの追加・削除はメインスレッドで実行
- ロックフリーで高速なアクセスを実現

### 8.2 DownTrack内のReceiver参照

```go
// downtrack.go:57-99
type DownTrack struct {
    // ...
    receiver       Receiver  // Receiverへのインターフェース参照★
    codec          webrtc.RTPCodecCapability
    // ...
}
```

## 9. 重要な設計パターン

### 9.1 プッシュモデル（Push Model）

**Receiverがアクティブにパケットをプッシュ：**

```
✅ Receiver → DownTrack (push)
   - Receiverがループ内でdt.WriteRTP()を呼び出し

❌ DownTrack → Receiver (pull) ではない
   - DownTrackはパケットを要求しない
   - 受動的に受信のみ
```

**利点：**

- DownTrackは状態管理に集中できる
- Receiverが配信タイミングを完全制御
- バックプレッシャー処理が容易

### 9.2 ファンアウトパターン（Fan-out Pattern）

**1つのソースから複数の宛先へ：**

```
       [Receiver]
         /  |  \
        /   |   \
       /    |    \
   DT1    DT2   DT3 ... DTN
```

**実装：**

```go
for _, dt := range w.downTracks[layer].Load().([]*DownTrack) {
    dt.WriteRTP(pkt, layer)  // 各DownTrackに同じパケットを送信
}
```

### 9.3 コールバックパターン

**DownTrack → Receiverの通信はコールバックベース：**

```go
// DownTrack内
d.receiver.SendRTCP(fwdPkts)           // RTCP転送
d.receiver.RetransmitPackets(d, pkts)  // 再送要求
```

## 10. パフォーマンスとスレッドセーフティ

### 10.1 並行実行

```
[Receiver]
├── writeRTP(0) ← goroutine 1 (Layer 0)
├── writeRTP(1) ← goroutine 2 (Layer 1)
├── writeRTP(2) ← goroutine 3 (Layer 3)
└── nackWorker  ← worker pool (パケット再送)

[DownTrack 1..N]
├── handleRTCP() ← RTCPリーダーゴルーチン
└── handleLayerChange() ← 適応的調整
```

### 10.2 ロック戦略

**Receiver:**

```go
// downTracksはatomic.Valueを使用（ロックフリー読み取り）
dts := w.downTracks[layer].Load().([]*DownTrack)

// 更新時のみロック
w.Lock()
w.storeDownTrack(layer, track)
w.Unlock()
```

**DownTrack:**

```go
// 主にatomic操作を使用
atomic.LoadInt32(&d.currentSpatialLayer)
atomic.StoreInt32(&d.targetSpatialLayer, targetLayer)
```

## 11. エラー処理とリカバリ

### 11.1 配信エラー

```go
// receiver.go:402-410
for _, dt := range w.downTracks[layer].Load().([]*DownTrack) {
    if err = dt.WriteRTP(pkt, layer); err != nil {
        if err == io.EOF || err == io.ErrClosedPipe {
            // DownTrackが閉じられた場合、リストから削除
            w.Lock()
            w.deleteDownTrack(layer, dt.id)
            w.Unlock()
        }
        Logger.Error(err, "Error writing to down track", "id", dt.id)
    }
}
```

**特徴：**

- エラーは個別に処理（1つのDownTrackのエラーが他に影響しない）
- 自動的に接続解除とクリーンアップ

### 11.2 再送エラー

```go
// receiver.go:318-361
w.nackWorker.Submit(func() {
    for _, meta := range packets {
        // パケット取得試行
        i, err := buff.GetPacket(pktBuff, meta.sourceSeqNo)
        if err != nil {
            if err == io.EOF {
                break  // Buffer終了
            }
            continue  // このパケットはスキップ、次を試行
        }
        // 再送処理...
    }
})
```

## まとめ

### 関係性の本質

1. **所有関係**:
   - Receiver: DownTrackリストを所有・管理
   - DownTrack: Receiverへの参照のみ保持

2. **データフロー**:
   - **主フロー**: Receiver → DownTrack (RTPパケット配信)
   - **制御フロー**: DownTrack → Receiver (RTCP、再送要求)

3. **多重度**:
   - 1つのReceiver : N個のDownTrack (N = Subscriber数)
   - Simulcast時は各レイヤーが独立したDownTrackリストを保持

4. **設計パターン**:
   - プッシュモデル（Receiverがアクティブ）
   - ファンアウトパターン（1対多配信）
   - コールバックベース（逆方向通信）

5. **スレッドセーフティ**:
   - atomic.Valueによるロックフリー読み取り
   - 更新時のみロック取得
   - レイヤーごとに独立したゴルーチン
