# ion-sfu アーキテクチャとフロー詳細解説

## 概要

ion-sfuは、WebRTC Selective Forwarding Unit (SFU)のGo実装です。SFUはメディアストリームをトランスコードせずに、複数のピア間でRTPパケットをルーティング・転送します。

## コア概念とアーキテクチャ

### 1. SFU (Selective Forwarding Unit)

**責務**: システム全体のエントリーポイントとライフサイクル管理

- すべてのセッションを管理
- WebRTC設定（ICE、STUN/TURN）の初期化
- オプショナルなTURNサーバーの起動
- データチャネルの登録

**ファイル**: `pkg/sfu/sfu.go`

**構造**:

```go
type SFU struct {
    webrtc       WebRTCTransportConfig  // ピア接続作成用の設定
    turn         *turn.Server           // オプションのTURNサーバー
    sessions     map[string]Session     // セッションマップ
    datachannels []*Datachannel        // 登録されたデータチャネル
    withStats    bool                   // 統計収集フラグ
}
```

### 2. Session

**責務**: ピアのグループ化とメディアルーティングスコープの提供

- セッション内のピア管理（追加・削除・検索）
- パブリッシャーのメディアをサブスクライバーに配信
- 音声レベル監視（誰が話しているか）
- データチャネルのファンアウト

**ファイル**: `pkg/sfu/session.go`

**構造**:

```go
type SessionLocal struct {
    id           string                    // セッションID
    peers        map[string]Peer           // ピアマップ
    relayPeers   map[string]*RelayPeer    // リレーピア（SFU間通信用）
    audioObs     *AudioObserver           // 音声レベル監視
    fanOutDCs    []string                 // ファンアウトデータチャネルラベル
    datachannels []*Datachannel          // データチャネルミドルウェア
}
```

### 3. Peer

**責務**: クライアント接続の抽象化（Pub/Subモデルの実装）

各ピアは通常2つのWebRTC PeerConnectionを持ちます：

- **Publisher接続**: ピア→SFUへのメディア送信
- **Subscriber接続**: SFU→ピアへのメディア送信

**ファイル**: `pkg/sfu/peer.go`

**構造**:

```go
type PeerLocal struct {
    id         string
    session    Session
    publisher  *Publisher    // メディア送信用
    subscriber *Subscriber   // メディア受信用
    
    OnOffer                    func(*webrtc.SessionDescription)
    OnIceCandidate             func(*webrtc.ICECandidateInit, int)
    OnICEConnectionStateChange func(webrtc.ICEConnectionState)
}
```

### 4. Publisher

**責務**: ピアからのメディア受信とセッション内への配信

- 着信メディアトラックの検出とReceiver作成
- Routerへのトラック登録
- セッション内の全サブスクライバーへの自動配信
- データチャネルの処理
- リレー機能（SFU間のメディア転送）

**ファイル**: `pkg/sfu/publisher.go`

**構造**:

```go
type Publisher struct {
    id         string
    pc         *webrtc.PeerConnection
    router     Router                    // メディアルーティング
    session    Session
    tracks     []PublisherTrack         // トラックとReceiverのペア
    relayPeers []*relayPeer             // リレーピア
}
```

### 5. Subscriber

**責務**: ピアへのメディア配信管理

- DownTrackの管理（送信トラック）
- SDPネゴシエーション（デバウンス付き）
- データチャネル管理
- RTCP Sender Reportの定期送信

**ファイル**: `pkg/sfu/subscriber.go`

**構造**:

```go
type Subscriber struct {
    id       string
    pc       *webrtc.PeerConnection
    tracks   map[string][]*DownTrack   // ストリームIDごとのダウントラック
    channels map[string]*webrtc.DataChannel
    negotiate func()                    // デバウンスされたネゴシエーション
}
```

### 6. Router

**責務**: メディアストリームのルーティングロジック

- Receiver管理（トラックID検索、Simulcastサポート）
- DownTrack作成と管理
- RTCPフィードバック処理（PLI/FIR/TWCC）
- 統計収集

**ファイル**: `pkg/sfu/router.go`

**構造**:

```go
type router struct {
    id            string
    twcc          *twcc.Responder        // Transport-wide CC
    rtcpCh        chan []rtcp.Packet     // RTCPチャネル
    session       Session
    receivers     map[string]Receiver    // トラックIDでインデックス
    bufferFactory *buffer.Factory
}
```

### 7. Receiver

**責務**: 着信RTPストリームの処理とダウントラックへの配信

- RTPパケットの受信と配信
- Simulcast/SVCレイヤー管理（最大3レイヤー: q, h, f）
- NACK処理とパケット再送
- ダウントラック管理（レイヤーごと）

**ファイル**: `pkg/sfu/receiver.go`

**構造**:

```go
type WebRTCReceiver struct {
    peerID      string
    trackID     string
    streamID    string
    buffers     [3]*buffer.Buffer        // Simulcastレイヤーごと
    upTracks    [3]*webrtc.TrackRemote   // アップストリームトラック
    downTracks  [3]atomic.Value          // []*DownTrack
    nackWorker  *workerpool.WorkerPool   // NACK処理用
    isSimulcast bool
}
```

### 8. DownTrack

**責務**: サブスクライバーへのRTP送信

- RTPパケットの送信（シーケンス番号/タイムスタンプ調整）
- Simulcast処理（空間/時間レイヤー切り替え）
- 適応的ビットレート制御
- RTCP処理（PLI/FIR/NACK）

**ファイル**: `pkg/sfu/downtrack.go`

**構造**:

```go
type DownTrack struct {
    id                  string
    peerID              string
    ssrc                uint32
    sequencer           *sequencer
    trackType           DownTrackType      // Simple or Simulcast
    currentSpatialLayer int32
    targetSpatialLayer  int32
    temporalLayer       int32
    receiver            Receiver
    writeStream         webrtc.TrackLocalWriter
}
```

## フロー詳細

### 1. 初期化フロー

```mermaid
sequenceDiagram
    participant App as Application
    participant SFU as SFU
    participant Session as Session
    participant WebRTC as WebRTCTransportConfig

    App->>SFU: NewSFU(config)
    SFU->>WebRTC: NewWebRTCTransportConfig(config)
    WebRTC-->>SFU: WebRTCTransportConfig
    SFU->>SFU: Initialize sessions map
    SFU->>SFU: Start TURN server (optional)
    SFU-->>App: SFU instance
```

### 2. ピア参加フロー

```mermaid
sequenceDiagram
    participant Client as Client
    participant Peer as Peer
    participant SFU as SFU
    participant Session as Session
    participant Publisher as Publisher
    participant Subscriber as Subscriber

    Client->>Peer: Join(sessionID, peerID)
    Peer->>SFU: GetSession(sessionID)
    
    alt Session exists
        SFU-->>Peer: existing Session
    else Session does not exist
        SFU->>Session: newSession(sessionID)
        Session-->>SFU: new Session
        SFU-->>Peer: new Session
    end
    
    Peer->>Publisher: NewPublisher()
    Publisher-->>Peer: publisher
    
    Peer->>Subscriber: NewSubscriber()
    Subscriber-->>Peer: subscriber
    
    Peer->>Session: AddPeer(peer)
    Session->>Session: Add to peers map
    
    Session->>Session: Subscribe(peer)
    Session->>Session: Subscribe to existing publishers
```

### 3. メディア配信フロー（Publish & Subscribe）

```mermaid
sequenceDiagram
    participant P1 as Publisher Peer
    participant Pub as Publisher
    participant Router as Router
    participant Recv as Receiver
    participant Session as Session
    participant DT as DownTrack
    participant Sub as Subscriber
    participant P2 as Subscriber Peer

    Note over P1,Pub: Publisher receives media
    P1->>Pub: OnTrack(track, receiver)
    Pub->>Router: AddReceiver(receiver, track)
    Router->>Recv: NewWebRTCReceiver()
    Router->>Recv: AddUpTrack(track, buffer)
    Router-->>Pub: receiver
    
    Note over Pub,Session: Publish to session
    Pub->>Session: Publish(router, receiver)
    
    loop For each subscriber in session
        Session->>Router: AddDownTracks(subscriber, receiver)
        Router->>DT: NewDownTrack()
        Router->>Sub: AddDownTrack(streamID, downTrack)
        Router->>Recv: AddDownTrack(downTrack)
        Sub->>Sub: negotiate()
    end
    
    Note over Recv,P2: Media forwarding loop
    loop Continuous
        Recv->>Recv: ReadExtended() from buffer
        Recv->>DT: WriteRTP(packet, layer)
        DT->>DT: Adjust SN/TS/SSRC
        DT->>Sub: WriteRTP to writeStream
        Sub->>P2: RTP packets
    end
```

### 4. Simulcast レイヤー切り替えフロー

```mermaid
sequenceDiagram
    participant Client as Client
    participant DT as DownTrack
    participant Recv as Receiver
    participant Buffer as Buffer[layer]

    Note over Client,DT: Client requests layer switch
    Client->>DT: SwitchSpatialLayer(targetLayer)
    DT->>DT: Check current != target
    DT->>Recv: SwitchDownTrack(downTrack, targetLayer)
    
    Recv->>Recv: pending[layer].set(true)
    Recv->>Recv: Add to pendingTracks[layer]
    
    Note over Recv,Buffer: Wait for keyframe
    loop Until keyframe
        Buffer->>Recv: ReadExtended()
        alt Not keyframe
            Recv->>Recv: Send PLI
        else Keyframe received
            Recv->>Recv: Move downTrack to new layer
            Recv->>DT: SwitchSpatialLayerDone(layer)
            DT->>DT: currentSpatialLayer = layer
        end
    end
```

### 5. RTCP フィードバックフロー

```mermaid
sequenceDiagram
    participant Sub as Subscriber
    participant DT as DownTrack
    participant Recv as Receiver
    participant Pub as Publisher
    participant Client as Publisher Client

    Note over Sub,DT: Client sends RTCP
    Sub->>DT: handleRTCP(bytes)
    
    alt PLI (Picture Loss Indication)
        DT->>DT: Update MediaSSRC
        DT->>Recv: SendRTCP(PLI)
        Recv->>Pub: rtcpCh <- PLI
        Pub->>Client: WriteRTCP(PLI)
    else NACK (packet loss)
        DT->>DT: Extract lost packets
        DT->>Recv: RetransmitPackets(packets)
        Recv->>Recv: Get from buffer
        Recv->>DT: Resend packets
    else REMB/RR (bandwidth estimation)
        DT->>DT: handleLayerChange()
        alt High packet loss (>25%)
            DT->>DT: Switch to lower layer
        else Low packet loss (<5%) & bandwidth available
            DT->>DT: Switch to higher layer
        end
    end
```

### 6. データチャネルフロー

```mermaid
sequenceDiagram
    participant P1 as Peer 1
    participant Pub as Publisher
    participant Session as Session
    participant Sub as Subscriber
    participant P2 as Peer 2

    Note over P1,Pub: Create DataChannel
    P1->>Pub: OnDataChannel(dc)
    Pub->>Session: AddDatachannel(ownerID, dc)
    
    Session->>Session: Add to fanOutDCs
    
    loop For each other peer
        Session->>Sub: AddDataChannel(label)
        Sub->>Sub: pc.CreateDataChannel()
        Sub->>Sub: negotiate()
    end
    
    Note over P1,P2: Message fanout
    P1->>Pub: Send message on dc
    Pub->>Session: FanOutMessage(origin, label, msg)
    
    loop For each peer except origin
        Session->>Sub: GetDataChannel(label)
        Sub->>P2: SendText/Send(msg)
    end
```

### 7. NACK処理とパケット再送フロー

```mermaid
sequenceDiagram
    participant Client as Subscriber Client
    participant DT as DownTrack
    participant Seq as Sequencer
    participant Recv as Receiver
    participant Buf as Buffer
    participant Worker as NACKWorker

    Client->>DT: RTCP NACK(lost packets)
    DT->>Seq: getSeqNoPairs(packetList)
    Seq-->>DT: []packetMeta
    
    DT->>Recv: RetransmitPackets(downTrack, packets)
    Recv->>Worker: Submit(retransmit job)
    
    Worker->>Worker: For each packet
    Worker->>Buf: GetPacket(seqNo)
    Buf-->>Worker: RTP packet
    
    Worker->>Worker: Adjust header (SN, TS, SSRC, PT)
    Worker->>Worker: Modify VP8 temporal payload (if needed)
    Worker->>DT: WriteRTP(packet)
    DT->>Client: Retransmitted packet
```

### 8. 音声レベル監視フロー

```mermaid
sequenceDiagram
    participant Recv as Receiver
    participant Buf as Buffer
    participant AO as AudioObserver
    participant Session as Session
    participant DC as DataChannel
    participant Clients as All Clients

    Note over Recv,Buf: Audio RTP received
    Recv->>Buf: Write audio packet
    Buf->>Buf: Calculate audio level
    Buf->>AO: observe(streamID, level)
    
    Note over Session: Periodic calculation (e.g., 1000ms)
    loop Every interval
        Session->>AO: Calc()
        AO->>AO: Apply threshold & filter
        AO-->>Session: Audio levels JSON
        
        Session->>Session: Marshal to JSON
        Session->>Session: GetDataChannels(APIChannelLabel)
        
        loop For each API channel
            Session->>DC: SendText(audioLevels)
            DC->>Clients: Audio levels notification
        end
    end
```

## 全体フロー図

```mermaid
graph TB
    subgraph "SFU Container"
        SFU[SFU]
        Session1[Session 1]
        Session2[Session N]
        SFU --> Session1
        SFU --> Session2
    end
    
    subgraph "Session Scope"
        Session1 --> Peer1[Peer A]
        Session1 --> Peer2[Peer B]
        Session1 --> AudioObs[AudioObserver]
    end
    
    subgraph "Peer Structure"
        Peer1 --> Pub1[Publisher]
        Peer1 --> Sub1[Subscriber]
    end
    
    subgraph "Publisher Side"
        Pub1 --> Router1[Router]
        Router1 --> Recv1[Receiver]
        Router1 --> Recv2[Receiver]
        Recv1 --> Buf1[Buffer Layer 0]
        Recv1 --> Buf2[Buffer Layer 1]
        Recv1 --> Buf3[Buffer Layer 2]
    end
    
    subgraph "Subscriber Side"
        Sub1 --> DT1[DownTrack 1]
        Sub1 --> DT2[DownTrack 2]
        DT1 --> Recv1
        DT2 --> Recv2
    end
    
    subgraph "WebRTC Transport"
        PC1[PeerConnection Publisher]
        PC2[PeerConnection Subscriber]
        Pub1 --> PC1
        Sub1 --> PC2
    end
    
    Client1[Client A] -.->|Media In| PC1
    PC2 -.->|Media Out| Client1
```

## コンポーネント相互作用マトリックス

| From/To | SFU | Session | Peer | Publisher | Subscriber | Router | Receiver | DownTrack |
|---------|-----|---------|------|-----------|------------|--------|----------|-----------|
| **SFU** | - | Create, Manage | - | - | - | - | - | - |
| **Session** | Register/Unregister | - | Add, Remove, Subscribe | Publish to | - | - | - | - |
| **Peer** | GetSession | Join, Leave | - | Create | Create | - | - | - |
| **Publisher** | - | Publish, AddDatachannel | - | - | - | AddReceiver | - | - |
| **Subscriber** | - | - | Negotiate | - | - | AddDownTracks | - | AddDownTrack |
| **Router** | - | - | - | - | Create DownTracks | - | AddReceiver, AddDownTrack | Create |
| **Receiver** | - | AudioObserver | - | - | - | RTCP | - | WriteRTP, RetransmitPackets |
| **DownTrack** | - | - | - | - | Negotiate | - | SendRTCP, SwitchLayer | - |

## Simulcast レイヤー管理

### レイヤー定義

```text
Layer 2 (High): Full resolution (f)
Layer 1 (Mid):  Half resolution (h)
Layer 0 (Low):  Quarter resolution (q)
```

### レイヤー切り替え条件

**アップスイッチ（より高品質へ）**:

- パケットロス ≤ 5%
- 利用可能帯域幅 ≥ 現在のビットレートの150%
- スイッチディレイ経過（頻繁な切り替え防止）

**ダウンスイッチ（より低品質へ）**:

- パケットロス ≥ 25%
- 利用可能帯域幅 ≤ 現在のビットレートの62.5%

### VP8 時間レイヤーフィルタリング

DownTrackはVP8のTL0PICIDXとPictureIDを使用して時間レイヤーをフィルタリングします：

```text
TL2: ●━━●━━●━━●━━●  (すべてのフレーム)
TL1: ●━━━━━●━━━━━●  (1/2のフレーム)
TL0: ●━━━━━━━━━━━●  (1/4のフレーム - ベースレイヤー)
```

## パケットフロー概要

### パブリッシャー → サブスクライバー

```text
Client A (Publisher)
    ↓ RTP
PeerConnection (Publisher)
    ↓ OnTrack
Publisher.OnTrack
    ↓ AddReceiver
Router
    ↓ Create
Receiver
    ↓ AddUpTrack
Buffer[layer] (NACK, Reorder, Stats)
    ↓ ReadExtended
Receiver.writeRTP
    ↓ WriteRTP
DownTrack[per subscriber]
    ↓ Adjust (SN, TS, SSRC)
DownTrack.writeStream
    ↓ RTP
PeerConnection (Subscriber)
    ↓ RTP
Client B (Subscriber)
```

## バッファとパケット処理

### Buffer の責務

`pkg/buffer/` パッケージは以下を提供：

- **パケットバッファリング**: 受信パケットの保存とリオーダリング
- **NACK処理**: 失われたパケットの検出と再送要求
- **統計収集**: ビットレート、パケットロス、ジッター
- **TWCC処理**: Transport-wide Congestion Control

### パケット処理パイプライン

```text
RTP Packet
    ↓
Buffer.Write()
    ↓
Store in bucket (bucket = SN % bucketSize)
    ↓
Update statistics (bitrate, timestamp)
    ↓
Detect audio level (for audio)
    ↓
TWCC callback (for video)
    ↓
Buffer.ReadExtended()
    ↓
ExtPacket (with metadata)
```

## RTCP処理

### フィードバックタイプ

**PLI (Picture Loss Indication)**:

- クライアントがデコードエラーを検出
- DownTrack → Receiver → Publisher → Source Client

**FIR (Full Intra Request)**:

- 完全なキーフレーム要求
- 処理フローはPLIと同様

**NACK (Negative Acknowledgement)**:

- 特定パケットの再送要求
- Sequencerがソースシーケンス番号にマッピング
- Bufferから再送

**REMB (Receiver Estimated Maximum Bitrate)**:

- クライアントの推定受信帯域幅
- レイヤー切り替えの判断に使用

**TWCC (Transport-wide Congestion Control)**:

- パケット到着時刻ベースの輻輳制御
- Responderがフィードバックを生成

## データチャネル

### タイプ

**API Channel** (`ion-sfu`):

- Publisher側で終端
- 音声レベル通知などのシグナリング用

**ファンアウトチャネル**:

- セッション内の全ピアに配信
- カスタムアプリケーションメッセージ用

**ミドルウェアチェーン**:

- データチャネルメッセージにミドルウェアパターンを適用
- 認証、検証、ロギングなどに使用可能

### ミドルウェア処理フロー

```text
Message Received
    ↓
Middleware 1 (e.g., Auth)
    ↓
Middleware 2 (e.g., Validation)
    ↓
Middleware N (e.g., Logging)
    ↓
onMessage handler
    ↓
FanOut (if configured)
```

## リレー機能（SFU間通信）

### リレーピア

SFUは他のSFUインスタンスとカスケード接続可能：

```text
SFU A (Publisher's SFU)
    ↓ Relay Peer
SFU B (Remote SFU)
    ↓ RelayPeer in Session
Subscribers in SFU B
```

### リレートラック作成

1. Publisher.Relay() が呼ばれる
2. relay.Peer 作成
3. 各PublisherTrackに対してDownTrackを作成
4. relay.Peer.AddTrack()
5. 定期的なSender Report送信（オプション）

## 統計収集

### 収集される統計

- **Stream統計**: ビットレート、パケット数、バイト数
- **ドリフト計算**: 複数ストリーム間の同期ずれ
- **CNAME**: Sender Reportから抽出
- **グローバル統計**: セッション数、ピア数、トラック数

### 統計の使用箇所

- レイヤー切り替え判断
- QoSモニタリング
- デバッグとトラブルシューティング

## 最適化とパフォーマンス

### パケットファクトリー

```go
packetFactory = &sync.Pool{
    New: func() interface{} {
        b := make([]byte, 1460)
        return &b
    },
}
```

頻繁なメモリアロケーションを防ぐためのバイトバッファプール。

### バラストメモリ

GCヒューリスティックを調整し、GC頻度を減少：

```go
ballast := make([]byte, config.Ballast*1024*1024)
runtime.KeepAlive(ballast)
```

### ネゴシエーションのデバウンス

Subscriberは250msのデバウンスを使用し、複数のトラック追加を1回のネゴシエーションにバッチ処理。

### NACKワーカープール

Receiverはワーカープールを使用してNACK処理を非同期化。

## エラーハンドリングと回復

### ICE接続状態

```text
Connected → Disconnected
    ↓ (timeout)
Failed/Closed
    ↓
Peer.Close()
    ↓
Session.RemovePeer()
    ↓
(Session empty?) → Session.Close()
```

### キーフレーム待機

レイヤー切り替えやリシンク時、DownTrackはキーフレームを待機：

- キーフレームでない場合、パケットを破棄しPLI送信
- キーフレーム受信でスイッチを完了

## セキュリティ考慮事項

- **DTLS**: WebRTCの組み込み暗号化
- **TURN認証**: カスタム認証関数サポート
- **データチャネルミドルウェア**: メッセージ検証とフィルタリング
- **リソース制限**: MaxPacketTrack などの制限

## まとめ

ion-sfuは以下の階層で構成されます：

1. **SFU**: システムエントリーポイント
2. **Session**: ピアグループとルーティングスコープ
3. **Peer**: クライアント抽象化（Publisher + Subscriber）
4. **Publisher**: メディア受信とルーティング
5. **Subscriber**: メディア配信
6. **Router**: ルーティングロジック
7. **Receiver**: 着信ストリーム処理
8. **DownTrack**: 送信トラック（適応的制御付き）

各コンポーネントは明確な責務を持ち、疎結合で拡張可能なアーキテクチャを実現しています。Simulcastサポート、適応的ビットレート制御、効率的なパケット処理により、スケーラブルなWebRTC SFUを提供します。
