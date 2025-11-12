# DownTrackとSubscriberの関係の詳細解説

## 結論

**DownTrackとSubscriberは1:1の関係ではありません。**

正確な関係は：

```
1つのSubscriber : 複数のDownTrack (1:N)
1つのDownTrack  : 1つのSubscriber (N:1)
```

**つまり、1つのSubscriberは複数のDownTrackを所有します。**

## 1. データ構造から見る関係性

### 1.1 Subscriberの構造

```go
// subscriber.go:45-60
type Subscriber struct {
    sync.RWMutex
    
    id string
    pc *webrtc.PeerConnection
    me *webrtc.MediaEngine
    
    tracks     map[string][]*DownTrack  // ★重要★
    channels   map[string]*webrtc.DataChannel
    candidates []webrtc.ICECandidateInit
    
    negotiate func()
    closeOnce sync.Once
    
    noAutoSubscribe bool
}
```

**重要なポイント：**

```go
tracks map[string][]*DownTrack
       ↑         ↑
       |         └─ DownTrackの配列（複数保持）
       └─ streamID（キー）
```

- **`map[string][]*DownTrack`** は「streamIDごとにDownTrackの配列を持つ」という意味
- 1つのSubscriberが複数のDownTrackを管理できる構造

### 1.2 DownTrackの構造

```go
// downtrack.go:57-99
type DownTrack struct {
    id            string
    peerID        string  // ★Subscriber ID★
    // ...
    receiver       Receiver
    transceiver    *webrtc.RTPTransceiver
    // ...
}
```

- `peerID`フィールドに**所属するSubscriberのID**を保持
- 1つのDownTrackは1つのSubscriberにのみ所属

## 2. なぜ1つのSubscriberが複数のDownTrackを持つのか？

### 2.1 Session内の複数Publisherからの受信

**典型的なシナリオ：ビデオ会議**

```
Session
├── Publisher A (ビデオ + オーディオ)
├── Publisher B (ビデオ + オーディオ)
├── Publisher C (ビデオ + オーディオ)
└── Subscriber D (受信専用)

Subscriber Dは全員から受信：
├── DownTrack 1 (Publisher Aのビデオ)
├── DownTrack 2 (Publisher Aのオーディオ)
├── DownTrack 3 (Publisher Bのビデオ)
├── DownTrack 4 (Publisher Bのオーディオ)
├── DownTrack 5 (Publisher Cのビデオ)
└── DownTrack 6 (Publisher Cのオーディオ)

合計：6つのDownTrack
```

### 2.2 双方向通信の場合

**一般的なピアツーピア会議：**

```
Peer A (Publisher + Subscriber)
├── Publisher側
│   └── Router A
│       ├── Receiver A1 (ビデオ)
│       └── Receiver A2 (オーディオ)
└── Subscriber側
    ├── DownTrack (Peer Bのビデオ) ← Receiver B1から
    ├── DownTrack (Peer Bのオーディオ) ← Receiver B2から
    ├── DownTrack (Peer Cのビデオ) ← Receiver C1から
    └── DownTrack (Peer Cのオーディオ) ← Receiver C2から

Peer A.Subscriber: 4つのDownTrack
```

### 2.3 具体例：3人の会議

```
Session: "meeting-room-123"

Peer Alice
├── Publisher (送信)
│   ├── ビデオトラック → Receiver_Alice_Video
│   └── オーディオトラック → Receiver_Alice_Audio
└── Subscriber (受信) ← 4つのDownTrack
    ├── DownTrack_Bob_Video
    ├── DownTrack_Bob_Audio
    ├── DownTrack_Carol_Video
    └── DownTrack_Carol_Audio

Peer Bob
├── Publisher (送信)
│   ├── ビデオトラック → Receiver_Bob_Video
│   └── オーディオトラック → Receiver_Bob_Audio
└── Subscriber (受信) ← 4つのDownTrack
    ├── DownTrack_Alice_Video
    ├── DownTrack_Alice_Audio
    ├── DownTrack_Carol_Video
    └── DownTrack_Carol_Audio

Peer Carol
├── Publisher (送信)
│   ├── ビデオトラック → Receiver_Carol_Video
│   └── オーディオトラック → Receiver_Carol_Audio
└── Subscriber (受信) ← 4つのDownTrack
    ├── DownTrack_Alice_Video
    ├── DownTrack_Alice_Audio
    ├── DownTrack_Bob_Video
    └── DownTrack_Bob_Audio
```

**まとめ：**

- 各Peerの**Subscriberは4つのDownTrack**を持つ（自分以外の2人 × 2トラック）
- 各Peerの**Publisherから2つのReceiver**が作成される

## 3. DownTrack管理の実装

### 3.1 DownTrackの追加

```go
// subscriber.go:173-182
func (s *Subscriber) AddDownTrack(streamID string, downTrack *DownTrack) {
    s.Lock()
    defer s.Unlock()
    
    // streamIDごとにDownTrackを配列に追加
    if dt, ok := s.tracks[streamID]; ok {
        // 既存のstreamIDに追加
        dt = append(dt, downTrack)
        s.tracks[streamID] = dt
    } else {
        // 新しいstreamID
        s.tracks[streamID] = []*DownTrack{downTrack}
    }
}
```

**動作例：**

```go
// Publisher Aのトラック追加
subscriber.AddDownTrack("stream-A", downTrack_A_Video)
subscriber.AddDownTrack("stream-A", downTrack_A_Audio)

// Publisher Bのトラック追加
subscriber.AddDownTrack("stream-B", downTrack_B_Video)
subscriber.AddDownTrack("stream-B", downTrack_B_Audio)

// 結果：
// subscriber.tracks = {
//   "stream-A": [downTrack_A_Video, downTrack_A_Audio],
//   "stream-B": [downTrack_B_Video, downTrack_B_Audio],
// }
```

### 3.2 DownTrackの取得

```go
// subscriber.go:260-264
func (s *Subscriber) GetDownTracks(streamID string) []*DownTrack {
    s.RLock()
    defer s.RUnlock()
    return s.tracks[streamID]  // 特定のstreamIDのDownTrack配列を返す
}

// subscriber.go:250-258
func (s *Subscriber) DownTracks() []*DownTrack {
    s.RLock()
    defer s.RUnlock()
    var downTracks []*DownTrack
    for _, tracks := range s.tracks {
        downTracks = append(downTracks, tracks...)  // すべてのDownTrackを返す
    }
    return downTracks
}
```

### 3.3 DownTrackの削除

```go
// subscriber.go:184-202
func (s *Subscriber) RemoveDownTrack(streamID string, downTrack *DownTrack) {
    s.Lock()
    defer s.Unlock()
    
    if dts, ok := s.tracks[streamID]; ok {
        // 配列から特定のDownTrackを削除
        idx := -1
        for i, dt := range dts {
            if dt == downTrack {
                idx = i
                break
            }
        }
        if idx >= 0 {
            // スワップして削除（順序は保証されない）
            dts[idx] = dts[len(dts)-1]
            dts[len(dts)-1] = nil
            dts = dts[:len(dts)-1]
            s.tracks[streamID] = dts
        }
    }
}
```

## 4. DownTrack作成フロー

### 4.1 新しいPublisherがSession参加時

```
1. Publisher A がセッションに参加
   ↓
2. Aがメディアトラックを送信開始
   ↓
3. Router A.AddReceiver() でReceiverを作成
   ↓
4. Session.Publish(router, receiver) が呼ばれる
   ↓
5. Session内の全Subscriberをループ (session.go:283-297)
   for _, p := range s.Peers() {
       router.AddDownTracks(p.Subscriber(), r)
   }
   ↓
6. 各SubscriberにDownTrackが追加される
   - Subscriber B: +1 DownTrack
   - Subscriber C: +1 DownTrack
   - Subscriber D: +1 DownTrack
```

### 4.2 新しいSubscriberがSession参加時

```
1. Peer E がセッションに参加
   ↓
2. Peer E.Join() → Session.Subscribe(peer) (session.go:300-360)
   ↓
3. Session内の全Publisherをループ
   for _, p := range peers {
       p.Publisher().GetRouter().AddDownTracks(peer.Subscriber(), nil)
   }
   ↓
4. Peer E.Subscriberに既存の全トラック用のDownTrackが作成される
   - Publisher Aのトラック → +2 DownTrack
   - Publisher Bのトラック → +2 DownTrack
   - Publisher Cのトラック → +2 DownTrack
   
   合計：Peer E.Subscriberは6つのDownTrackを持つ
```

## 5. データフロー

### 5.1 複数DownTrackへのRTP配信

**Subscriberは複数のDownTrackから同時にパケットを受信：**

```
[Receiver A1]           [Receiver B1]           [Receiver C1]
(Publisher A)           (Publisher B)           (Publisher C)
    ↓                       ↓                       ↓
[DownTrack A1]         [DownTrack B1]         [DownTrack C1]
    ↓                       ↓                       ↓
    └───────────────────────┴───────────────────────┘
                            ↓
                   [Subscriber.pc]
                   (WebRTC PeerConnection)
                            ↓
                   [WebRTC Client]
```

### 5.2 RTCP Reports送信

**Subscriberは定期的にすべてのDownTrackのSender Reportを送信：**

```go
// subscriber.go:276-318
func (s *Subscriber) downTracksReports() {
    for {
        time.Sleep(5 * time.Second)
        
        var r []rtcp.Packet
        var sd []rtcp.SourceDescriptionChunk
        
        s.RLock()
        // すべてのDownTrackをループ★
        for _, dts := range s.tracks {
            for _, dt := range dts {
                if !dt.bound.get() {
                    continue
                }
                // 各DownTrackからSender Reportを取得
                if sr := dt.CreateSenderReport(); sr != nil {
                    r = append(r, sr)
                }
                sd = append(sd, dt.CreateSourceDescriptionChunks()...)
            }
        }
        s.RUnlock()
        
        // 一括送信
        if err := s.pc.WriteRTCP(r); err != nil {
            Logger.Error(err, "Sending downtrack reports err")
        }
    }
}
```

## 6. streamIDによるグループ化

### 6.1 なぜstreamIDでグループ化するのか？

**同じPublisherのトラックをグループ化：**

```go
tracks map[string][]*DownTrack
       ↑         ↑
       |         └─ 同じPublisherの複数トラック
       └─ streamID（通常はPublisher識別子）
```

**例：**

```
subscriber.tracks = {
    "stream-Alice": [
        DownTrack_Alice_Video,
        DownTrack_Alice_Audio,
    ],
    "stream-Bob": [
        DownTrack_Bob_Video,
        DownTrack_Bob_Audio,
    ],
    "stream-Carol": [
        DownTrack_Carol_Video,
        DownTrack_Carol_Audio,
    ],
}
```

### 6.2 streamIDの使用例

```go
// 特定のPublisherのトラックのみ取得
aliceTracks := subscriber.GetDownTracks("stream-Alice")
// → [DownTrack_Alice_Video, DownTrack_Alice_Audio]

// Aliceが退出時、Aliceのトラックのみ削除
for _, dt := range aliceTracks {
    subscriber.RemoveDownTrack("stream-Alice", dt)
}
```

## 7. 比較：ReceiverとSubscriberの多重度

### 7.1 Receiver側（Publisher側）

```
1つのReceiver : N個のDownTrack
               (N = そのトラックをサブスクライブしているSubscriber数)

例：Publisher Aのビデオトラック
Receiver_A_Video
    ├→ DownTrack (Subscriber B用)
    ├→ DownTrack (Subscriber C用)
    ├→ DownTrack (Subscriber D用)
    └→ DownTrack (Subscriber E用)
```

### 7.2 Subscriber側（受信側）

```
1つのSubscriber : M個のDownTrack
                 (M = Session内のすべてのトラック数)

例：Subscriber Bが受信
Subscriber B
    ├─ DownTrack (Publisher Aのビデオ) ← Receiver_A_Video
    ├─ DownTrack (Publisher Aのオーディオ) ← Receiver_A_Audio
    ├─ DownTrack (Publisher Cのビデオ) ← Receiver_C_Video
    ├─ DownTrack (Publisher Cのオーディオ) ← Receiver_C_Audio
    └─ ... (他のPublisherのトラック)
```

### 7.3 全体像

```
         Publisher A                    Publisher B
              ↓                              ↓
    [Receiver_A_Video]             [Receiver_B_Video]
         /    |    \                    /    |    \
        /     |     \                  /     |     \
       /      |      \                /      |      \
      ↓       ↓       ↓              ↓       ↓       ↓
    DT1     DT2     DT3            DT4     DT5     DT6
     ↓       ↓       ↓              ↓       ↓       ↓
  Sub X   Sub Y   Sub Z          Sub X   Sub Y   Sub Z

Subscriber X:
├─ DT1 (A's Video)
└─ DT4 (B's Video)

Subscriber Y:
├─ DT2 (A's Video)
└─ DT5 (B's Video)

Subscriber Z:
├─ DT3 (A's Video)
└─ DT6 (B's Video)
```

## 8. DownTrack識別

### 8.1 一意性の保証

**DownTrackは以下の組み合わせで一意：**

```go
(subscriberID, streamID, trackID)

例：
- Subscriber: "peer-123"
- StreamID:   "stream-Alice"
- TrackID:    "video-track-1"

→ 一意のDownTrack: "peer-123" receiving "video-track-1" from "stream-Alice"
```

### 8.2 重複チェック

```go
// router.go:268-273
func (r *router) AddDownTrack(sub *Subscriber, recv Receiver) (*DownTrack, error) {
    // 既に存在するかチェック
    for _, dt := range sub.GetDownTracks(recv.StreamID()) {
        if dt.ID() == recv.TrackID() {
            return dt, nil  // 既存のDownTrackを返す
        }
    }
    
    // 新しいDownTrackを作成...
}
```

## 9. ライフサイクル例

### 9.1 シナリオ：4人会議の開始から終了まで

**初期状態：空のSession**

```
Session: "meeting"
Peers: []
```

**ステップ1：Aliceが参加**

```
Session: "meeting"
├── Peer Alice
    ├── Publisher (ビデオ+オーディオ)
    │   └── Router → 2 Receivers
    └── Subscriber
        └── DownTracks: 0個（他の参加者なし）
```

**ステップ2：Bobが参加**

```
Session: "meeting"
├── Peer Alice
│   └── Subscriber
│       └── DownTracks: 2個
│           ├── DownTrack (Bob's Video)
│           └── DownTrack (Bob's Audio)
└── Peer Bob
    └── Subscriber
        └── DownTracks: 2個
            ├── DownTrack (Alice's Video)
            └── DownTrack (Alice's Audio)
```

**ステップ3：Carolが参加**

```
Session: "meeting"
├── Peer Alice
│   └── Subscriber → DownTracks: 4個
│       ├── DownTrack (Bob's Video)
│       ├── DownTrack (Bob's Audio)
│       ├── DownTrack (Carol's Video)
│       └── DownTrack (Carol's Audio)
├── Peer Bob
│   └── Subscriber → DownTracks: 4個
│       ├── DownTrack (Alice's Video)
│       ├── DownTrack (Alice's Audio)
│       ├── DownTrack (Carol's Video)
│       └── DownTrack (Carol's Audio)
└── Peer Carol
    └── Subscriber → DownTracks: 4個
        ├── DownTrack (Alice's Video)
        ├── DownTrack (Alice's Audio)
        ├── DownTrack (Bob's Video)
        └── DownTrack (Bob's Audio)
```

**ステップ4：Daveが参加**

```
各Subscriberが持つDownTrack数: 6個
(3人の他の参加者 × 2トラック)
```

**ステップ5：Bobが退出**

```
- Bob's Publisher閉鎖 → Bob's Receivers削除
- 各SubscriberからBob関連のDownTrack削除
- 各Subscriberが持つDownTrack数: 4個に減少
```

## まとめ

### 関係性の本質

1. **DownTrack → Subscriber**: **N:1の関係**
   - 複数のDownTrackが1つのSubscriberに所属
   - 各DownTrackは`peerID`でSubscriberを識別

2. **Subscriber → DownTrack**: **1:Nの関係**
   - 1つのSubscriberが複数のDownTrackを所有
   - `tracks map[string][]*DownTrack`で管理

3. **DownTrack数の計算**:
   ```
   1つのSubscriberが持つDownTrack数 = 
       (Session内の他のPublisher数) × (各Publisherのトラック数)
   ```

4. **グループ化**:
   - streamIDでPublisherごとにグループ化
   - 同じPublisherのトラック（ビデオ+オーディオ）を一緒に管理

5. **ライフサイクル**:
   - Publisherが参加 → 既存のSubscriberに新しいDownTrack追加
   - Subscriberが参加 → 既存のすべてのトラック用にDownTrack作成
   - Publisherが退出 → すべてのSubscriberから該当DownTrack削除

**結論：DownTrackとSubscriberは1:1ではなく、1つのSubscriberが複数のDownTrackを持つ1:Nの関係です。**
