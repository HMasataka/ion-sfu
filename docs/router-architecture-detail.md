# Routerアーキテクチャの詳細解説

## Routerの役割

Routerは、ion-sfuにおいて**Publisherのメディアストリームを管理し、Receiverの生成、DownTrackの作成、RTCPフィードバックの集約**を担当するコアコンポーネントです。

## 1. Routerの生成数

### 基本原則

**1つのPublisherに対して1つのRouterが作成されます**

- Routerは**Publisher専用**のコンポーネントです
- クライアント数でもトラック数でもなく、**Publisherの数**に依存します
- 例：
  - 3つのPublisherがいる場合 → **3つのRouter**が作成されます
  - 1つのPublisherが複数のトラックを送信 → **1つのRouter**がすべてのトラックを管理

### 生成タイミングとコード

Routerは`publisher.go:102`でPublisher作成時に生成されます：

```go
// publisher.go:82-104
func NewPublisher(id string, session Session, cfg *WebRTCTransportConfig) (*Publisher, error) {
    // MediaEngineとPeerConnectionの初期化...

    p := &Publisher{
        id:      id,
        pc:      pc,
        cfg:     cfg,
        router:  newRouter(id, session, cfg),  // Routerを作成
        session: session,
    }

    return p, nil
}
```

### Router生成の詳細（`router.go:84-102`）

```go
func newRouter(id string, session Session, config *WebRTCTransportConfig) Router {
    ch := make(chan []rtcp.Packet, 10)
    r := &router{
        id:            id,                      // PublisherのIDと同じ
        rtcpCh:        ch,                      // RTCPパケット集約用チャネル
        stopCh:        make(chan struct{}),
        config:        config.Router,
        session:       session,                 // Sessionへの参照
        receivers:     make(map[string]Receiver), // trackID → Receiver
        stats:         make(map[uint32]*stats.Stream),
        bufferFactory: config.BufferFactory,
    }

    return r
}
```

## 2. Routerの主要な役割

### 2.1 Receiver管理

**Routerは、着信WebRTCトラックからReceiverオブジェクトを作成・管理します**

#### AddReceiver（`router.go:128-233`）

```go
func (r *router) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote,
    trackID, streamID string) (Receiver, bool) {

    // 1. BufferとRTCPリーダーのペアを作成
    buff, rtcpReader := r.bufferFactory.GetBufferPair(uint32(track.SSRC()))

    // 2. RTCPフィードバックをRouterのチャネルに集約
    buff.OnFeedback(func(fb []rtcp.Packet) {
        r.rtcpCh <- fb
    })

    // 3. トラックタイプ別の処理
    if track.Kind() == webrtc.RTPCodecTypeAudio {
        // 音声レベル監視を設定
        buff.OnAudioLevel(func(level uint8) {
            r.session.AudioObserver().observe(streamID, level)
        })
    } else if track.Kind() == webrtc.RTPCodecTypeVideo {
        // TWCCレスポンダーを初期化（ビデオの場合）
        if r.twcc == nil {
            r.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
        }
        buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
            r.twcc.Push(sn, timeNS, marker)
        })
    }

    // 4. Receiverを作成または取得
    recv, ok := r.receivers[trackID]
    if !ok {
        recv = NewWebRTCReceiver(receiver, track, r.id)
        r.receivers[trackID] = recv
        recv.SetRTCPCh(r.rtcpCh)  // RTCPチャネルを共有
        publish = true
    }

    // 5. UpTrackを追加（Simulcastレイヤーの追加）
    recv.AddUpTrack(track, buff, r.config.Simulcast.BestQualityFirst)

    // 6. BufferをWebRTCレシーバーにバインド
    buff.Bind(receiver.GetParameters(), buffer.Options{
        MaxBitRate: r.config.MaxBandwidth,
    })

    return recv, publish
}
```

**重要なポイント：**

- `receivers`マップ：trackID → Receiver
- 同じtrackIDの場合、既存のReceiverに新しいレイヤー（Simulcast）を追加
- すべてのReceiverは同じ`rtcpCh`を共有し、RTCPフィードバックを集約

### 2.2 DownTrack作成

**RouterはReceiverとSubscriberを接続するDownTrackを作成します**

#### AddDownTrack（`router.go:268-319`）

```go
func (r *router) AddDownTrack(sub *Subscriber, recv Receiver) (*DownTrack, error) {
    // 1. 重複チェック
    for _, dt := range sub.GetDownTracks(recv.StreamID()) {
        if dt.ID() == recv.TrackID() {
            return dt, nil  // 既に存在する場合はスキップ
        }
    }

    // 2. コーデックを登録
    codec := recv.Codec()
    if err := sub.me.RegisterCodec(codec, recv.Kind()); err != nil {
        return nil, err
    }

    // 3. DownTrackを作成
    downTrack, err := NewDownTrack(webrtc.RTPCodecCapability{
        MimeType:     codec.MimeType,
        ClockRate:    codec.ClockRate,
        Channels:     codec.Channels,
        SDPFmtpLine:  codec.SDPFmtpLine,
        RTCPFeedback: []webrtc.RTCPFeedback{{"goog-remb", ""}, {"nack", ""}, {"nack", "pli"}},
    }, recv, r.bufferFactory, sub.id, r.config.MaxPacketTrack)

    // 4. WebRTC Transceiverを追加
    downTrack.transceiver, err = sub.pc.AddTransceiverFromTrack(downTrack,
        webrtc.RTPTransceiverInit{
            Direction: webrtc.RTPTransceiverDirectionSendonly,
        })

    // 5. クローズハンドラー設定
    downTrack.OnCloseHandler(func() {
        if sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
            sub.pc.RemoveTrack(downTrack.transceiver.Sender())
            sub.RemoveDownTrack(recv.StreamID(), downTrack)
            sub.negotiate()
        }
    })

    // 6. Bind時の処理（RTCP送信開始）
    downTrack.OnBind(func() {
        go sub.sendStreamDownTracksReports(recv.StreamID())
    })

    // 7. SubscriberとReceiverに登録
    sub.AddDownTrack(recv.StreamID(), downTrack)
    recv.AddDownTrack(downTrack, r.config.Simulcast.BestQualityFirst)

    return downTrack, nil
}
```

#### AddDownTracks（一括追加）（`router.go:235-261`）

```go
func (r *router) AddDownTracks(s *Subscriber, recv Receiver) error {
    r.Lock()
    defer r.Unlock()

    // 自動サブスクライブがオフの場合はスキップ
    if s.noAutoSubscribe {
        return nil
    }

    // 特定のReceiverが指定されている場合
    if recv != nil {
        if _, err := r.AddDownTrack(s, recv); err != nil {
            return err
        }
        s.negotiate()
        return nil
    }

    // すべてのReceiverに対してDownTrackを作成
    if len(r.receivers) > 0 {
        for _, rcv := range r.receivers {
            if _, err := r.AddDownTrack(s, rcv); err != nil {
                return err
            }
        }
        s.negotiate()
    }
    return nil
}
```

### 2.3 RTCPフィードバック処理

**RouterはすべてのReceiver、Buffer、DownTrackからのRTCPパケットを集約し、Publisher PeerConnectionに送信します**

#### RTCPチャネルの設定（`router.go:263-266`）

```go
func (r *router) SetRTCPWriter(fn func(packet []rtcp.Packet) error) {
    r.writeRTCP = fn
    go r.sendRTCP()  // RTCPゴルーチンを起動
}
```

#### RTCP送信ループ（`router.go:331-342`）

```go
func (r *router) sendRTCP() {
    for {
        select {
        case pkts := <-r.rtcpCh:
            // すべてのRTCPパケットをPublisher PeerConnectionに書き込み
            if err := r.writeRTCP(pkts); err != nil {
                Logger.Error(err, "Write rtcp to peer err", "peer_id", r.id)
            }
        case <-r.stopCh:
            return
        }
    }
}
```

**RTCPソース：**

1. **Buffer** (`router.go:136-138`)
   - NACK（パケット再送要求）
   - PLI（Picture Loss Indication）

2. **TWCC Responder** (`router.go:149-151`)
   - Transport-Wide Congestion Control フィードバック

3. **Receiver** (Receiverから`rtcpCh`経由)
   - DownTrackからのRTCP要求を転送

### 2.4 TWCC（Transport-Wide Congestion Control）

**ビデオトラックの輻輳制御を実現します**

#### TWCCの初期化（`router.go:147-156`）

```go
if track.Kind() == webrtc.RTPCodecTypeVideo {
    // 最初のビデオトラックでTWCCレスポンダーを作成
    if r.twcc == nil {
        r.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
        r.twcc.OnFeedback(func(p rtcp.RawPacket) {
            r.rtcpCh <- []rtcp.Packet{&p}  // TWCCフィードバックをRTCPチャネルに送信
        })
    }
    // BufferからTWCC情報を受信
    buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
        r.twcc.Push(sn, timeNS, marker)  // パケット到着情報をプッシュ
    })
}
```

**TWCCの動作：**

1. Bufferがパケット到着時刻とシーケンス番号を記録
2. TWCCレスポンダーがこの情報を集約
3. 定期的にRTCPフィードバックパケット生成
4. PublisherがこのフィードバックをSubscriber（受信側）に送信
5. Subscriberが送信レートを調整

### 2.5 統計収集とドリフト計算

**Router内のすべてのストリームの統計とメディア同期情報を管理します**

#### Sender Report処理（`router.go:182-189`）

```go
case *rtcp.SenderReport:
    // NTPとRTPタイムスタンプの関係を保存
    buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)

    // 統計を更新
    if r.config.WithStats {
        if st := r.stats[pkt.SSRC]; st != nil {
            r.updateStats(st)  // ドリフト計算を実行
        }
    }
```

#### ドリフト計算（`router.go:344-399`）

```go
func (r *router) updateStats(stream *stats.Stream) {
    // 同じCNAME（同じクライアント）のすべてのストリームから
    // 最小・最大NTPタイムスタンプを計算
    cname := stream.GetCName()
    minPacketNtpTime, maxPacketNtpTime := calculateLatestMinMaxSenderNtpTime(cname)

    // ドリフト = 音声と映像の同期ずれ
    driftInMillis := maxPacketNtpTime - minPacketNtpTime

    // すべてのストリームにドリフト情報を設定
    setDrift(cname, driftInMillis)

    stream.CalcStats()
}
```

**ドリフト計算の目的：**

- 音声と映像の同期ずれを検出
- リップシンク（lip-sync）問題の診断
- 複数のストリーム間のタイミング調整

## 3. RouterとPeerの関係

### 3.1 Peerの構造

**1つのPeerは通常2つのPeerConnectionを持ちます：**

```
PeerLocal (peer.go:88-104)
├── Publisher (メディア送信用)
│   ├── PeerConnection (WebRTC)
│   └── Router (このPublisher専用)
│       └── receivers (trackID → Receiver)
└── Subscriber (メディア受信用)
    ├── PeerConnection (WebRTC)
    └── tracks (streamID → []*DownTrack)
```

### 3.2 Router作成フロー

```
1. PeerLocal.Join() (peer.go:114)
   ↓
2. NewPublisher() (publisher.go:83)
   ↓
3. newRouter(id, session, cfg) (router.go:84)
   - Publisherと同じIDを持つRouterを作成
   - Sessionへの参照を保持
```

### 3.3 複数ピアのアーキテクチャ

```
Session
├── Peer A
│   ├── Publisher A
│   │   └── Router A (Receiver A1, A2, ...)
│   └── Subscriber A
│       └── DownTracks (from Router B, C, ...)
├── Peer B
│   ├── Publisher B
│   │   └── Router B (Receiver B1, B2, ...)
│   └── Subscriber B
│       └── DownTracks (from Router A, C, ...)
└── Peer C
    ├── Publisher C
    │   └── Router C (Receiver C1, C2, ...)
    └── Subscriber C
        └── DownTracks (from Router A, B, ...)
```

**メディアフロー：**

```
Peer A (Publisher)
  → Router A
    → Receiver A1, A2
      → Session.Publish()
        → Router A.AddDownTracks(Peer B.Subscriber)
        → Router A.AddDownTracks(Peer C.Subscriber)
          → DownTrack B1, C1 created
```

## 4. データフロー全体像

### 4.1 新しいトラック受信時のフロー

```
1. Publisher.pc.OnTrack() トリガー (publisher.go:106)
   ↓
2. Router.AddReceiver() (router.go:128)
   - Buffer作成
   - Receiver作成または取得
   - RTCPチャネル設定
   - TWCC設定（ビデオの場合）
   ↓
3. Session.Publish(router, receiver) (session.go:283)
   - セッション内のすべてのSubscriberをループ
   ↓
4. Router.AddDownTracks(subscriber, receiver) (router.go:235)
   ↓
5. Router.AddDownTrack(subscriber, receiver) (router.go:268)
   - DownTrack作成
   - Transceiverをサブスクライバーに追加
   - Receiver.AddDownTrack()を呼び出し
   ↓
6. Subscriber.negotiate() (subscriber.go:267)
   - 250msのデバウンス後にOffer生成
```

### 4.2 実行時のRTPフロー

```
Publisher (WebRTC)
  ↓ RTP packets
[Buffer] ← bind to RTPReceiver
  ↓ feedback
[Router.rtcpCh] ← NACK, PLI, TWCC
  ↓ ReadExtended()
[Receiver.writeRTP()]
  ↓ for each DownTrack
[DownTrack.WriteRTP()]
  ↓ write to transceiver
Subscriber (WebRTC)
```

### 4.3 RTCPフロー

```
DownTrack (Subscriber側)
  ↓ PLI, NACK, REMB
[Receiver.rtcpCh] ← SendRTCP()
  ↓
[Router.rtcpCh] ← 集約
  ↓
Publisher.pc.WriteRTCP()
  ↓ RTCP packets
Publisher (WebRTC client)
```

### 4.4 TWCCフロー（ビデオ輻輳制御）

```
Subscriber (WebRTC)
  ↓ RTP packets with TWCC extension
[Buffer] ← OnTransportWideCC()
  ↓ sequence number + timestamp
[Router.twcc] ← Push(sn, timeNS, marker)
  ↓ periodically
[Router.twcc.OnFeedback()] ← TWCC RTCP packet
  ↓
[Router.rtcpCh]
  ↓
Publisher.pc.WriteRTCP()
  ↓ Transport-CC feedback
Publisher (WebRTC client)
  ↓ adjust bitrate
```

## 5. Routerの重要な設計パターン

### 5.1 RTCPチャネルの集約

**すべてのRTCPフィードバックを単一チャネルに集約：**

- **利点：**
  - RTCPパケットのバッチ送信が可能
  - WebRTC接続への書き込み回数を削減
  - ロック競合の最小化

```go
// 複数のソースから単一チャネルへ
Buffer.OnFeedback()        → rtcpCh
TWCC.OnFeedback()          → rtcpCh
Receiver.SendRTCP()        → rtcpCh
                             ↓
                        sendRTCP() goroutine
                             ↓
                     Publisher.pc.WriteRTCP()
```

### 5.2 Receiver管理のマップ構造

```go
receivers map[string]Receiver  // trackID → Receiver
```

**設計理由：**

- trackIDによる高速検索（O(1)）
- Simulcastの場合、同じtrackIDで複数のレイヤーを管理
- トラックの追加・削除が頻繁に発生

### 5.3 Session参照の保持

Routerは`session`への参照を保持：

```go
type router struct {
    session       Session
    // ...
}
```

**用途：**

- AudioObserverへのアクセス（`router.go:142-144`）
- セッション全体の統計収集
- ピア間のメディア配信調整

## 6. Routerのライフサイクル

### 6.1 作成

```go
// Publisher作成時にRouterも作成
p := &Publisher{
    router: newRouter(id, session, cfg),
}

// RTCPライターを設定（sendRTCP goroutine起動）
p.router.SetRTCPWriter(p.pc.WriteRTCP)
```

### 6.2 実行中

```go
// トラック受信ごとにReceiverを追加
router.AddReceiver(receiver, track, trackID, streamID)

// 新しいSubscriberが参加時にDownTrackを作成
router.AddDownTracks(subscriber, nil)
```

### 6.3 停止

```go
// Publisher.Close() → router.Stop()
func (r *router) Stop() {
    r.stopCh <- struct{}{}  // sendRTCP goroutineを停止

    if r.config.WithStats {
        stats.Peers.Dec()
    }
}
```

## 7. 重要なコード参照

### 7.1 Router作成

- `router.go:84-102` - newRouter()
- `publisher.go:83-163` - NewPublisher()

### 7.2 Receiver管理

- `router.go:128-233` - AddReceiver()
- `router.go:321-329` - deleteReceiver()

### 7.3 DownTrack作成

- `router.go:268-319` - AddDownTrack()
- `router.go:235-261` - AddDownTracks()

### 7.4 RTCPフィードバック

- `router.go:263-266` - SetRTCPWriter()
- `router.go:331-342` - sendRTCP()
- `router.go:136-156` - TWCC設定

### 7.5 統計とドリフト

- `router.go:344-399` - updateStats()
- `router.go:182-189` - Sender Report処理

## まとめ

1. **Routerの生成数**: **1つのPublisherに対して1つ**（トラック数やクライアント数ではない）

2. **Routerの役割**:
   - **Receiver生成と管理**（trackIDごと）
   - **DownTrack作成**（ReceiverとSubscriberを接続）
   - **RTCPフィードバック集約**（すべてのソースから）
   - **TWCC処理**（ビデオ輻輳制御）
   - **統計収集とドリフト計算**（メディア同期）

3. **アーキテクチャ階層**:

   ```
   Session
   ├── Peer → Publisher → Router → Receiver → DownTrack → Subscriber
   ```

4. **データフロー**:
   - **RTP**: Publisher → Buffer → Receiver → DownTrack → Subscriber
   - **RTCP**: DownTrack/Buffer/TWCC → Router.rtcpCh → Publisher

5. **設計パターン**:
   - RTCPチャネルの集約による効率化
   - trackIDベースのReceiver管理
   - Sessionへの参照保持
