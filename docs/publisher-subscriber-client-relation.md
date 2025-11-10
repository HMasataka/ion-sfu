# Publisher、Subscriber、Clientの関係

## 基本的な関係

**1つのクライアント = 1つのPeer = 2つのPeerConnection（Publisher + Subscriber）**

```text
┌─────────────────────────────────────────────────────────────┐
│  ブラウザ（Client）                                          │
│  ┌────────────────┐                                          │
│  │ カメラ/マイク  │                                          │
│  └───────┬────────┘                                          │
│          │                                                   │
│          ↓ (送信)                                            │
│  ┌──────────────────┐          ┌──────────────────┐         │
│  │ Publisher        │◄────────►│ Subscriber       │         │
│  │ PeerConnection   │          │ PeerConnection   │         │
│  └──────────────────┘          └──────────────────┘         │
│          │                              ↓ (受信)            │
│          │                     ┌────────────────┐           │
│          │                     │ <video>要素    │           │
│          │                     └────────────────┘           │
└──────────┼─────────────────────────────────────────────────┘
           │
           │ WebSocket (JSON-RPC)
           │
┌──────────┼─────────────────────────────────────────────────┐
│  SFU     ↓                                                  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Peer (pkg/sfu/peer.go)                               │  │
│  │                                                       │  │
│  │  ┌─────────────────┐      ┌──────────────────┐      │  │
│  │  │ Publisher       │      │ Subscriber       │      │  │
│  │  │ (メディア受信)   │      │ (メディア送信)   │      │  │
│  │  └────────┬────────┘      └────────▲─────────┘      │  │
│  └───────────┼────────────────────────┼─────────────────┘  │
│              │                        │                    │
│              ↓                        │                    │
│         ┌─────────┐              ┌─────────┐              │
│         │ Router  │──────────────►│DownTrack│              │
│         └─────────┘              └─────────┘              │
└─────────────────────────────────────────────────────────────┘
```

## 詳細説明

### Client（クライアント）

- **役割**: ブラウザやアプリケーション上で動作するエンドユーザーのアプリケーション
- **実装**: `examples/echotest-jsonrpc/index.html`
- **機能**:
  - カメラ/マイクからメディアを取得
  - Publisherを通じてSFUにメディアを送信
  - Subscriberを通じて他のクライアントのメディアを受信

### Peer（ピア）

**ファイル**: `pkg/sfu/peer.go:60-77`

```go
type PeerLocal struct {
    id         string
    session    Session
    publisher  *Publisher   // メディア送信用
    subscriber *Subscriber  // メディア受信用
    // ...
}
```

- **役割**: SFU側でクライアントを表現するオブジェクト
- **特徴**:
  - 各ピアは**必ず2つのPeerConnection**を持つ
  - PublisherとSubscriberを統括管理

### Publisher（パブリッシャー）

**ファイル**: `pkg/sfu/publisher.go:18-35`

```go
type Publisher struct {
    id      string
    pc      *webrtc.PeerConnection  // クライアント→SFU方向
    router  Router
    tracks  []PublisherTrack
    // ...
}
```

- **役割**: クライアントからSFUへのメディア送信を担当
- **方向**: クライアント → SFU
- **処理内容**:
  1. クライアントのカメラ/マイク映像を受信
  2. RTPパケットを受信してバッファに格納
  3. Routerに渡して他のピアに配信

**フロー**:

```text
Client(カメラ) → Publisher.OnTrack() → Router.AddReceiver() → Session.Publish()
```

### Subscriber（サブスクライバー）

**ファイル**: `pkg/sfu/subscriber.go:16-31`

```go
type Subscriber struct {
    id       string
    pc       *webrtc.PeerConnection  // SFU→クライアント方向
    tracks   map[string][]*DownTrack
    channels map[string]*webrtc.DataChannel
    // ...
}
```

- **役割**: SFUからクライアントへのメディア送信を担当
- **方向**: SFU → クライアント
- **処理内容**:
  1. 他のクライアントからのメディアを受け取る
  2. DownTrackを通じてこのクライアントに配信
  3. 帯域幅に応じてサイマルキャスト層を選択

**フロー**:

```text
他のPublisher → Router → DownTrack → Subscriber → Client(video要素)
```

## 実際の動作例

### シナリオ: クライアントA、B、Cが参加

```text
┌─────────────┐
│ Client A    │
│ ┌──┐  ┌──┐ │
│ │Pub│ │Sub│ │───┐
│ └──┘  └──┘ │   │
└─────────────┘   │
                  │    ┌──────────────────────────┐
┌─────────────┐   │    │ SFU                      │
│ Client B    │   │    │  ┌─────┐                │
│ ┌──┐  ┌──┐ │───┼───►│  │Sess │                │
│ │Pub│ │Sub│ │   │    │  │ion  │                │
│ └──┘  └──┘ │   │    │  └──┬──┘                │
└─────────────┘   │    │     │                    │
                  │    │  ┌──▼───┐  ┌────────┐   │
┌─────────────┐   │    │  │Peer A│  │Peer B  │   │
│ Client C    │   │    │  │      │  │        │   │
│ ┌──┐  ┌──┐ │───┘    │  │Pub   │  │Pub Sub │   │
│ │Pub│ │Sub│ │        │  │Sub   │  │        │   │
│ └──┘  └──┘ │        │  └──────┘  └────────┘   │
└─────────────┘        │  ┌────────┐             │
                       │  │Peer C  │             │
                       │  │Pub Sub │             │
                       │  └────────┘             │
                       └──────────────────────────┘
```

### 具体的な流れ

#### 1. Client Aがメディアを配信

**ファイル**: `pkg/sfu/peer.go:87-191`

```go
p.Join(sessionID, userID, config)
  → Publisher作成 (peer.go:151-180)
  → Subscriber作成 (peer.go:107-148)
```

#### 2. Client AのカメラがPublisherに送信

**ファイル**: `pkg/sfu/publisher.go:77-106`

```go
pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
    r, pub := p.router.AddReceiver(receiver, track, ...)
    p.session.Publish(p.router, r)  // 他のピアに配信開始
})
```

#### 3. Client BとCのSubscriberがAの映像を受信

**ファイル**: `pkg/sfu/session.go:222-236`

```go
func (s *SessionLocal) Publish(router Router, r Receiver) {
    for _, p := range s.Peers() {  // 全ピアに配信
        router.AddDownTracks(p.Subscriber(), r)
    }
}
```

#### 4. Client Bのブラウザで表示

**ファイル**: `examples/echotest-jsonrpc/index.html:396-400`

```javascript
clientRemote.ontrack = (track, stream) => {
  remoteVideo.srcObject = stream; // Client Aの映像を表示
};
```

## なぜ2つのPeerConnectionが必要か？

**ファイル**: `pkg/sfu/peer.go:13-16`

```go
const (
    publisher  = 0  // クライアント→SFU
    subscriber = 1  // SFU→クライアント
)
```

### 理由

1. **方向の分離**: 送信と受信を独立して制御
2. **ネゴシエーションの簡素化**:
   - 新しいピアが参加 → Subscriberのみ再ネゴシエーション
   - Publisherは影響を受けない
3. **帯域幅制御の柔軟性**: 送受信で別々の帯域幅管理

### 実装例

**ファイル**: `pkg/sfu/peer.go:239-255`

```go
func (p *PeerLocal) Trickle(candidate webrtc.ICECandidateInit, target int) error {
    switch target {
    case publisher:   // クライアント→SFU
        p.publisher.AddICECandidate(candidate)
    case subscriber:  // SFU→クライアント
        p.subscriber.AddICECandidate(candidate)
    }
}
```

## データフロー詳細

### メディア送信フロー（Client → SFU → 他のClients）

```text
[Client A]
   │
   │ getUserMedia()
   │
   ↓
[Publisher PeerConnection A]
   │
   │ WebRTC (RTP)
   │
   ↓
[SFU - Publisher A] (pkg/sfu/publisher.go)
   │
   │ OnTrack()
   │
   ↓
[Router] (pkg/sfu/router.go)
   │
   │ AddReceiver()
   ├─→ [Buffer] パケット管理
   │   └─→ NACK, PLI, REMB処理
   │
   │ Session.Publish()
   │
   ├──────────────────┬──────────────────┐
   │                  │                  │
   ↓                  ↓                  ↓
[DownTrack B]    [DownTrack C]    [DownTrack D]
   │                  │                  │
   ↓                  ↓                  ↓
[Subscriber B]   [Subscriber C]   [Subscriber D]
   │                  │                  │
   ↓                  ↓                  ↓
[Client B]       [Client C]       [Client D]
```

### シグナリングフロー（JSON-RPC）

```text
[Client]                [JSON-RPC Server]           [Peer]
   │                           │                       │
   │  join(offer) ────────────►│                       │
   │                           │  NewPeer() ──────────►│
   │                           │                       │
   │                           │                    [Publisher]
   │                           │                       │
   │                           │                    [Subscriber]
   │                           │                       │
   │                           │◄────── answer ────────│
   │◄────────── answer ────────│                       │
   │                           │                       │
   │  trickle(ice) ───────────►│  AddICECandidate() ──►│
   │◄────── trickle(ice) ──────│◄────── OnICE ─────────│
   │                           │                       │
```

## コンポーネント間の責任

### Peer (pkg/sfu/peer.go)

- **責任**: PublisherとSubscriberのライフサイクル管理
- **主要メソッド**:
  - `Join()`: セッションへの参加、PublisherとSubscriber作成
  - `Answer()`: Offerへの応答
  - `Trickle()`: ICE候補の追加
  - `Close()`: リソースのクリーンアップ

### Publisher (pkg/sfu/publisher.go)

- **責任**: クライアントからのメディア受信とRouterへの渡し
- **主要機能**:
  - `OnTrack()`: RTPトラック受信時の処理
  - `Answer()`: WebRTC Offerへの応答
  - `AddICECandidate()`: ICE候補の追加

### Subscriber (pkg/sfu/subscriber.go)

- **責任**: クライアントへのメディア送信
- **主要機能**:
  - `AddDownTrack()`: 配信トラックの追加
  - `RemoveDownTrack()`: 配信トラックの削除
  - `CreateOffer()`: 再ネゴシエーション用Offerの作成
  - `negotiate()`: デバウンスされたネゴシエーション

### Router (pkg/sfu/router.go)

- **責任**: Receiver（入力）からDownTrack（出力）へのメディアルーティング
- **主要機能**:
  - `AddReceiver()`: 入力ストリームの登録
  - `AddDownTracks()`: 出力ストリームの作成と接続
  - RTCP フィードバックループの管理

## まとめ

### コンポーネント対応表

| 要素           | 役割                    | 方向                   | 数                    | 実装ファイル                           |
| -------------- | ----------------------- | ---------------------- | --------------------- | -------------------------------------- |
| **Client**     | ブラウザアプリ          | -                      | 複数                  | `examples/echotest-jsonrpc/index.html` |
| **Peer**       | SFU側のクライアント表現 | -                      | クライアントごとに1つ | `pkg/sfu/peer.go`                      |
| **Publisher**  | メディア受信            | Client → SFU           | Peerごとに1つ         | `pkg/sfu/publisher.go`                 |
| **Subscriber** | メディア送信            | SFU → Client           | Peerごとに1つ         | `pkg/sfu/subscriber.go`                |
| **Router**     | メディアルーティング    | Publisher → Subscriber | Peerごとに1つ         | `pkg/sfu/router.go`                    |
| **Session**    | ピアのグループ管理      | -                      | セッションIDごとに1つ | `pkg/sfu/session.go`                   |

### 重要なポイント

1. **Pub/Subモデル**: 各クライアントは必ず2つのPeerConnectionを持つ
2. **方向の分離**: 送信（Publisher）と受信（Subscriber）を独立管理
3. **効率的なルーティング**: SFUがメディアをトランスコードせず転送のみ
4. **柔軟な制御**: 帯域幅、サイマルキャスト層、接続状態を個別管理

この設計により、SFUは効率的に複数のクライアント間でメディアをルーティングし、スケーラブルなビデオ会議システムを実現しています。
