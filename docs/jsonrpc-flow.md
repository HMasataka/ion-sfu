# ion-sfu JSON-RPCフロードキュメント

## 概要

このドキュメントでは、JSON-RPCシグナリングプロトコルを使用した際のion-sfuの完全なフローについて説明します。JSON-RPC実装は、クライアントとSFU（Selective Forwarding Unit）間のWebRTCピア接続のためのWebSocketベースのシグナリングメカニズムを提供します。

## アーキテクチャコンポーネント

### 主要コンポーネント

- **JSON-RPCシグナリングサーバー** (`cmd/signal/json-rpc/`)
- **SFUコア** (`pkg/sfu/`)
- **WebRTCクライアント** (ブラウザベース)

## 完全なフロー

### 1. サーバー初期化

**ファイル**: `cmd/signal/json-rpc/main.go`

1. **設定の読み込み** (54-96行目)

- `config.toml`から設定を読み込み
- WebRTC設定（ICEサーバー、ポート範囲、タイムアウト）の解析
- 指定された詳細レベルでロガーを初期化

2. **SFUインスタンスの作成** (155行目)

- 設定を使用してSFUインスタンスを作成: `sfu.NewSFU(conf)`
- WebRTCトランスポート設定の初期化
- RTPパケット管理用のバッファファクトリの設定
- 有効な場合はTURNサーバーを初期化

3. **DataChannelの設定** (156-157行目)

- ラベル "ion-sfu" でAPIデータチャネルを作成
- クライアント制御用のSubscriberAPIミドルウェアを登録

4. **WebSocketサーバーの設定** (159-179行目)

- CORS有効化でWebSocket upgraderを作成
- クライアント接続用に `/ws` エンドポイントを登録
- 各WebSocket接続で新しいピアハンドラを生成

5. **メトリクスサーバー** (181行目)

- ポート8100でPrometheusメトリクスサーバーを起動
- `/metrics` エンドポイントを公開

### 2. クライアント接続フロー

**ファイル**: `examples/echotest-jsonrpc/index.html`

1. **WebSocket接続** (355-360行目)

```javascript
const signal = new Signal.IonSFUJSONRPCSignal("ws://localhost:7000/ws");
const client = new IonSDK.Client(signal);
signal.onopen = () => client.join("ion");
```

2. **メディアキャプチャ** (371-377行目)

- ユーザーメディア（カメラ/マイク）をリクエスト
- 解像度とサイマルキャスト設定でローカルストリームを作成
- ローカルビデオのプレビューを表示

### 3. シグナリングフロー（Join）

**フロー**: クライアント → WebSocket → JSON-RPCサーバー → SFU

#### クライアントサイド (365-366行目)

```javascript
signal.onopen = () => client.join("ion");
```

#### サーバーサイド

**ファイル**: `cmd/signal/json-rpc/main.go` (167-178行目)

1. **WebSocketアップグレード**

- HTTP接続をWebSocketにアップグレード
- WebSocket上でJSON-RPC接続を作成

2. **ピアの作成** (174行目)

```go
p := server.NewJSONSignal(sfu.NewPeer(s), logger)
```

3. **JSON-RPCハンドラの設定** (177行目)

- ハンドラ付きでJSON-RPC接続を作成
- 切断通知を待機

**ファイル**: `cmd/signal/json-rpc/server/server.go` (42-134行目)

#### Joinリクエストの処理 (52-88行目)

1. **Joinメッセージの解析** (53-59行目)

- セッションID（SID）を抽出
- ユーザーID（UID）を抽出
- WebRTC Offer SDPを抽出
- Join設定を抽出

2. **コールバックの設定** (61-74行目)

- `OnOffer`: 再ネゴシエーション用にクライアントへofferを送信
- `OnIceCandidate`: クライアントへICE候補を送信

3. **セッションへの参加** (76行目)

```go
err = p.Join(join.SID, join.UID, join.Config)

```

**ファイル**: `pkg/sfu/peer.go` (87-191行目)

#### ピアJoinプロセス

1. **ユーザーIDの生成** (98-101行目)

- 提供されていない場合はユニークIDを生成

2. **セッションの取得または作成** (104-105行目)

```go
s, cfg := p.provider.GetSession(sid)

```

**ファイル**: `pkg/sfu/sfu.go` (254-260行目)

```go
func (s *SFU) GetSession(sid string) (Session, WebRTCTransportConfig) {
    session := s.getSession(sid)
    if session == nil {
        session = s.newSession(sid)
    }
    return session, s.webrtc
}
```

3. **Create Subscriber** (lines 107-148)

- Create subscriber peer connection
- Setup negotiation needed handler
- Setup ICE candidate handler

4. **Create Publisher** (lines 151-180)

- Create publisher peer connection
- Setup ICE candidate handler
- Setup ICE connection state change handler

**File**: `pkg/sfu/publisher.go` (lines 54-134)

#### Publisher Creation

1. **Create Peer Connection** (lines 55-67)

- Get publisher media engine
- Create WebRTC API with settings
- Create peer connection

2. **Create Router** (line 73)

- Initialize router for media routing

3. **Setup Track Handler** (lines 77-106)

- Handle incoming remote tracks
- Add receiver to router
- Publish track to session
- Store track information

4. **Setup DataChannel Handler** (lines 108-114)

- Handle incoming data channels
- Add to session for fan-out

**File**: `pkg/sfu/subscriber.go` (lines 34-75)

#### Subscriber Creation

1. **Create Peer Connection** (lines 35-46)

- Get subscriber media engine
- Create peer connection

2. **Setup ICE Handler** (lines 57-70)

- Monitor connection state
- Close on failure or disconnection

3. **Start Reports** (line 72)

- Start background goroutine for RTCP reports

4. **Add Peer to Session** (line 183)

```go
p.session.AddPeer(p)

```

**File**: `pkg/sfu/session.go` (lines 87-91)

6. **Subscribe to Existing Streams** (line 188)

```go
p.session.Subscribe(p)

```

#### Answerの生成 (82-86行目)

**ファイル**: `cmd/signal/json-rpc/server/server.go`

```go
answer, err := p.Answer(join.Offer)
```

**ファイル**: `pkg/sfu/peer.go` (194-213行目)

1. **リモートDescriptionの設定** (205行目)

- クライアントのofferを適用

2. **Answerの作成** (205行目)

- パブリッシャーからSDP answerを生成

3. **Answerの返却** (88行目)

- JSON-RPC経由でクライアントにanswerを送信

### 4. メディア配信フロー

**フロー**: クライアント → Publisherピア接続 → Router → Receivers

#### クライアントサイド

**ファイル**: `examples/echotest-jsonrpc/index.html` (383行目)

```javascript
clientLocal.publish(media);
```

#### サーバーサイド

**ファイル**: `pkg/sfu/publisher.go` (77-106行目)

1. **OnTrackコールバック**

- RTPトラックが到着したときにトリガー

2. **Receiverの追加** (86行目)

```go
r, pub := p.router.AddReceiver(receiver, track, track.ID(), track.StreamID())

```

**ファイル**: `pkg/sfu/router.go` (99-204行目)

#### Router AddReceiverプロセス

1. **バッファペアの作成** (105行目)

- パケット管理用のRTPバッファを作成
- フィードバック用のRTCPリーダーを作成

2. **バッファコールバックの設定** (107-127行目)

- `OnFeedback`: RTCPフィードバックパケットを送信
- `OnAudioLevel`: オーディオレベルを追跡
- `OnTransportWideCC`: 輻輳制御のためのTWCCを処理

3. **Receiverの作成** (164-168行目)

- WebRTCレシーバーをラップ
- ルーターのレシーバーマップに保存
- クローズハンドラを設定

4. **バッファのバインド** (191-193行目)

- 帯域幅制限でバッファを設定

5. **セッションへの公開** (88行目)

```go
p.session.Publish(p.router, r)

```

**ファイル**: `pkg/sfu/session.go` (220-236行目)

#### セッションPublishプロセス

1. **ピアの反復処理** (223行目)

- 自身とサブスクライバーのないピアをスキップ

2. **DownTracksの追加** (231行目)

```go
router.AddDownTracks(p.Subscriber(), r)

```

**ファイル**: `pkg/sfu/router.go` (206-232行目)

#### AddDownTracksプロセス

1. **自動サブスクライブのチェック** (210-213行目)

- ピアが自動サブスクリプションを無効化している場合はスキップ

2. **各Receiverに対して** (223-229行目)

- 各サブスクライバーにdowntrackを作成
- サブスクライバーのトラックリストに追加

3. **ネゴシエーションのトリガー** (229行目)

- サブスクライバーに再ネゴシエーションを要求

### 5. メディアサブスクリプションフロー

**フロー**: Receiver → DownTrack → Subscriberピア接続 → クライアント

**ファイル**: `pkg/sfu/session.go` (239-299行目)

#### Subscribeプロセス

1. **既存ピアの取得** (240-250行目)

- セッション内のすべてのパブリッシャーを収集

2. **データチャネルへのサブスクライブ** (252-278行目)

- ファンアウト用のデータチャネルを追加

3. **パブリッシャーストリームへのサブスクライブ** (280-287行目)

```go
p.Publisher().GetRouter().AddDownTracks(peer.Subscriber(), nil)

```

4. **ネゴシエーションのトリガー** (298行目)

- サブスクライバーにofferの作成を要求

**File**: `pkg/sfu/peer.go` (lines 115-136)

#### Negotiation Process

1. **Debounced Trigger** (line 109-114)

- Prevent excessive renegotiation

2. **Create Offer** (line 125)

```go
offer, err := p.subscriber.CreateOffer()

```

3. **Send Offer to Client** (line 134)

```go
p.OnOffer(&offer)

```

**File**: `cmd/signal/json-rpc/server/server.go` (lines 61-66)

```go
p.OnOffer = func(offer *webrtc.SessionDescription) {
conn.Notify(ctx, "offer", offer)
}
```

#### Client Handles Offer

**File**: `examples/echotest-jsonrpc/index.html` (lines 396-414)

1. **Receive Track Event** (line 396)

```javascript
clientRemote.ontrack = (track, stream) => {

```

2. **Display Remote Video** (lines 398-400)

```javascript
remoteStream = stream;
remoteVideo.srcObject = stream;
```

3. **Answer Offer** (automatically handled by ion-sdk)

**File**: `cmd/signal/json-rpc/server/server.go` (lines 106-118)

#### Server Handles Answer

```go
case "answer":
err = p.SetRemoteDescription(negotiation.Desc)
```

**File**: `pkg/sfu/peer.go` (lines 216-236)

1. **Set Remote Description** (line 224)
2. **Clear Answer Pending Flag** (line 228)
3. **Trigger Pending Negotiation** (lines 230-233)

### 6. ICE Candidate Exchange

**File**: `cmd/signal/json-rpc/server/server.go` (lines 120-133)

#### Trickle ICE Process

1. **Receive Candidate** (lines 120-127)

```go
case "trickle":
   var trickle Trickle
   json.Unmarshal(*req.Params, &trickle)

```

2. **Add to PeerConnection** (line 129)

```go
err = p.Trickle(trickle.Candidate, trickle.Target)

```

**File**: `pkg/sfu/peer.go` (lines 239-255)

```go
func (p *PeerLocal) Trickle(candidate webrtc.ICECandidateInit, target int) error {
switch target {
case publisher:
    p.publisher.AddICECandidate(candidate)
case subscriber:
    p.subscriber.AddICECandidate(candidate)
}
}
```

### 7. Data Channel Communication

**File**: `examples/echotest-jsonrpc/index.html`

#### Client Creates Data Channel (line 386)

```javascript
localDataChannel = clientLocal.createDataChannel("data");
```

**File**: `pkg/sfu/session.go` (lines 158-218)

#### Server Handles Data Channel

1. **Add to Session** (line 158-171)

- Check if already exists
- Setup fan-out message handler

2. **Register with Owner** (line 175)
3. **Propagate to Other Peers** (lines 181-217)

- Create data channel on each subscriber
- Setup bidirectional message forwarding

### 8. Media Routing (Continuous)

**File**: `pkg/sfu/router.go`

#### Ongoing Processes

1. **RTCP Feedback Loop** (lines 302-313)

- Send RTCP packets to publisher
- Handle NACK, PLI, REMB requests

2. **Buffer Management** (`pkg/buffer/`)

- Reorder packets
- Handle packet loss with NACK
- Maintain jitter buffer

3. **DownTrack Sending** (`pkg/sfu/downtrack.go`)

- Forward RTP packets to subscribers
- Apply simulcast/SVC layer selection
- Bandwidth adaptation

### 9. 切断フロー

**File**: `pkg/sfu/peer.go` (lines 274-294)

#### クライアントの切断

1. **Close WebSocket**

- Detected by JSON-RPC connection

2. **Close Peer** (line 177 in main.go)

```go
defer p.Close()

```

3. **Remove from Session** (line 283)

```go
p.session.RemovePeer(p)

```

4. **Close Publisher** (line 286)
5. **Close Subscriber** (line 289)

**File**: `pkg/sfu/session.go` (lines 142-156)

#### セッションのクリーンアップ

1. **Remove Peer** (line 147)
2. **Check Peer Count** (line 149)
3. **Close Session if Empty** (line 154)

**File**: `pkg/sfu/sfu.go` (lines 226-234)

```go
session.OnClose(func() {
    s.Lock()
    delete(s.sessions, id)
    s.Unlock()
})
```

## メッセージフロー図

```text
Client                     JSON-RPC Server           SFU Core                Session/Router
  |                              |                       |                        |
  |--WebSocket Connect---------->|                       |                        |
  |                              |                       |                        |
  |--join(sid, uid, offer)------>|                       |                        |
  |                              |--NewPeer()----------->|                        |
  |                              |                       |--GetSession()--------->|
  |                              |                       |<--session, config------|
  |                              |<--Peer----------------|                        |
  |                              |                       |                        |
  |                              |--Join()-------------->|                        |
  |                              |                       |--CreatePublisher()---->|
  |                              |                       |--CreateSubscriber()--->|
  |                              |                       |--AddPeer()------------>|
  |                              |                       |--Subscribe()---------->|
  |                              |                       |                        |
  |                              |--Answer(offer)------->|                        |
  |                              |<--answer--------------|                        |
  |<-answer----------------------|                       |                        |
  |                              |                       |                        |
  |--ICE candidates------------->|--Trickle()----------->|                        |
  |<-ICE candidates--------------|                       |                        |
  |                              |                       |                        |
  |==RTP packets================>|                       |--OnTrack()------------>|
  |                              |                       |                       |
  |                              |                       |                       --AddReceiver()
  |                              |                       |                       --Publish()
  |                              |                       |                        |
  |                              |<-offer (renegotiate)--|<--OnOffer()-----------|
  |<-offer-----------------------|                       |                        |
  |                              |                       |                        |
  |--answer--------------------->|--SetRemoteDescription->                        |
  |                              |                       |                        |
  |<==RTP packets================|                       |<==DownTrack============|
  |                              |                       |                        |
```

## 主要な設定ファイル

### config.toml

Key settings:

- **WebRTC ICE**: STUN/TURN servers, port ranges, single port mode
- **Router**: Simulcast settings, bandwidth limits, packet buffer size
- **Audio Observer**: Level thresholds and intervals
- **TURN**: TURN server configuration

## APIメソッド（JSON-RPC）

### クライアント → サーバー

1. **join**
   - Parameters: `{sid, uid, offer, config}`
   - Response: `answer` (WebRTC SDP answer)
   - Purpose: Join session and establish publisher connection

2. **offer**
   - Parameters: `{desc}` (WebRTC SDP offer)
   - Response: `answer`
   - Purpose: Renegotiate publisher connection

3. **answer**
   - Parameters: `{desc}` (WebRTC SDP answer)
   - Purpose: Complete subscriber renegotiation

4. **trickle**
   - Parameters: `{candidate, target}` (ICE candidate, 0=publisher/1=subscriber)
   - Purpose: Add ICE candidate to peer connection

### サーバー → クライアント（通知）

1. **offer**
   - Parameters: WebRTC SDP offer
   - Purpose: Request subscriber renegotiation (new tracks available)

2. **trickle**
   - Parameters: `{candidate, target}`
   - Purpose: Send ICE candidate to client

## ファイルリファレンス

### メインエントリーポイント

- `cmd/signal/json-rpc/main.go:138-194`

### シグナリングサーバー

- `cmd/signal/json-rpc/server/server.go:42-134`

### コアSFUコンポーネント

- `pkg/sfu/sfu.go:191-260` - SFU instance and session management
- `pkg/sfu/peer.go:80-312` - Peer lifecycle and signaling
- `pkg/sfu/publisher.go:54-180` - Publisher peer connection
- `pkg/sfu/subscriber.go:34-290` - Subscriber peer connection
- `pkg/sfu/session.go:52-416` - Session management and routing
- `pkg/sfu/router.go:55-370` - Media routing logic

### クライアント例

- `examples/echotest-jsonrpc/index.html:334-521`

## まとめ

The JSON-RPC flow in ion-sfu follows a clear pattern:

1. **Initialization**: Server starts with configuration and creates SFU instance
2. **Connection**: Client connects via WebSocket and establishes JSON-RPC communication
3. **Join**: Client sends join request with offer, server creates peer and returns answer
4. **Publisher Setup**: Client publishes media, server receives tracks via publisher connection
5. **Routing**: Router distributes media from receivers to downtracks
6. **Subscriber Setup**: Server sends offer to client for subscribing to remote streams
7. **Media Flow**: Bidirectional RTP flow with RTCP feedback
8. **Data Channels**: Optional data channel for control messages and data exchange
9. **Cleanup**: Graceful disconnection and resource cleanup

The architecture separates concerns clearly: JSON-RPC handles signaling, WebRTC handles media transport, and the SFU core manages routing and session logic.
