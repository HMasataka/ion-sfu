/*
【ファイル概要: sfu.go】
このファイルはion-sfuの中心となるSFU（Selective Forwarding Unit）の実装を提供します。

【主要な役割】
1. SFUインスタンスの作成と管理
  - WebRTC設定の初期化（ICE、STUN/TURN、ポート範囲など）
  - セッション管理（複数のピア接続をグループ化）
  - データチャネルの登録と管理

2. WebRTC Transport設定の構築
  - ICE設定（SinglePort/PortRangeモード、NAT 1:1 IP、mDNS）
  - SDPセマンティクス（Unified Plan、Plan B）
  - タイムアウト設定（切断、失敗、キープアライブ）
  - バッファファクトリーの統合

3. セッションライフサイクル管理
  - 新規セッションの作成と登録
  - セッションIDによる検索と取得
  - セッション終了時のクリーンアップ処理

4. オプションのTURNサーバー統合
  - TURNサーバーの初期化と起動
  - 認証メカニズムの設定

【アーキテクチャ上の位置づけ】
  - SFU構造体: SFUの最上位レベルのコンテナ。すべてのセッションを保持し、
    WebRTC設定とTURNサーバーへの参照を管理します。
  - Session: ピアのグループ。各セッションは独立したメディアルーティングスコープを持ちます。
  - WebRTCTransportConfig: ピア接続の作成に使用される設定のコレクション。

【使用例】
config := sfu.Config{...}
sfuInstance := sfu.NewSFU(config)
session, webrtcConfig := sfuInstance.GetSession("session-id")
*/
package sfu

import (
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/pion/ice/v2"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/stats"
	"github.com/pion/turn/v2"
	"github.com/pion/webrtc/v3"
)

/*
Logger はlogr.Loggerインターフェースの実装です。
提供されない場合、ログ出力は無効化されます（Discard）。
SFU全体のログ出力を制御するグローバル変数として機能します。
*/
var Logger logr.Logger = logr.Discard()

/*
ICEServerConfig はSTUN/TURNサーバーの設定を定義します。
- URLs: サーバーのURL（stun:やturn:プロトコル）
- Username: TURN認証用のユーザー名
- Credential: TURN認証用のパスワードまたはシークレット
複数のICEサーバーを設定することで、接続の冗長性とフォールバックを実現します。
*/
type ICEServerConfig struct {
	URLs       []string `mapstructure:"urls"`
	Username   string   `mapstructure:"username"`
	Credential string   `mapstructure:"credential"`
}

/*
Candidates はICE候補の生成に関する設定を保持します。
- IceLite: ICE Liteモードを有効にする（サーバー側の最適化）
- NAT1To1IPs: NATの背後にいる場合の公開IPアドレスのリスト
NAT1To1IPsを設定することで、SFUが直接インターネットに露出していない環境でも
正しいICE候補を生成できます。
*/
type Candidates struct {
	IceLite    bool     `mapstructure:"icelite"`
	NAT1To1IPs []string `mapstructure:"nat1to1"`
}

/*
WebRTCTransportConfig はピア接続作成時に使用される完全な設定セットです。
この構造体はSFUからピア接続を作成する際に渡され、一貫した設定を保証します。
- Configuration: WebRTCの基本設定（ICEサーバー、SDPセマンティクスなど）
- Setting: Pionの詳細設定（ポート範囲、UDPマルチプレクサなど）
- Router: メディアルーティングに関する設定
- BufferFactory: RTP/RTCPバッファの作成ファクトリー
*/
type WebRTCTransportConfig struct {
	Configuration webrtc.Configuration
	Setting       webrtc.SettingEngine
	Router        RouterConfig
	BufferFactory *buffer.Factory
}

/*
WebRTCTimeoutsConfig はWebRTC接続のタイムアウト値を定義します（単位：秒）。
- ICEDisconnectedTimeout: ICE切断と判断するまでの時間
- ICEFailedTimeout: ICE失敗と判断するまでの時間
- ICEKeepaliveInterval: キープアライブパケットの送信間隔
これらの値を調整することで、ネットワーク環境に応じた接続の安定性を最適化できます。
*/
type WebRTCTimeoutsConfig struct {
	ICEDisconnectedTimeout int `mapstructure:"disconnected"`
	ICEFailedTimeout       int `mapstructure:"failed"`
	ICEKeepaliveInterval   int `mapstructure:"keepalive"`
}

/*
WebRTCConfig はWebRTC関連のすべての設定を含む包括的な構造体です。
- ICESinglePort: 単一のUDPポートを使用する場合のポート番号（0の場合は無効）
- ICEPortRange: 使用するポート範囲（[開始ポート, 終了ポート]）
- ICEServers: STUN/TURNサーバーのリスト
- Candidates: ICE候補生成の設定
- SDPSemantics: SDPのセマンティクス（"unified-plan", "plan-b", "unified-plan-with-fallback"）
- MDNS: mDNS候補の有効化（ローカルネットワーク発見）
- Timeouts: 各種タイムアウト設定
*/
type WebRTCConfig struct {
	ICESinglePort int                  `mapstructure:"singleport"`
	ICEPortRange  []uint16             `mapstructure:"portrange"`
	ICEServers    []ICEServerConfig    `mapstructure:"iceserver"`
	Candidates    Candidates           `mapstructure:"candidates"`
	SDPSemantics  string               `mapstructure:"sdpsemantics"`
	MDNS          bool                 `mapstructure:"mdns"`
	Timeouts      WebRTCTimeoutsConfig `mapstructure:"timeouts"`
}

/*
Config はSFU全体の設定を定義するトップレベルの構造体です。
設定ファイル（config.toml）から読み込まれ、SFUインスタンスの初期化に使用されます。
- SFU.Ballast: GCの最適化のためのバラストメモリサイズ（MB単位）
- SFU.WithStats: 統計情報の収集を有効にするかどうか
- WebRTC: WebRTC関連の設定
- Router: メディアルーティング設定
- Turn: TURNサーバーの設定
- BufferFactory: カスタムバッファファクトリー（オプション）
- TurnAuth: カスタムTURN認証関数（オプション）
*/
type Config struct {
	SFU struct {
		Ballast   int64 `mapstructure:"ballast"`
		WithStats bool  `mapstructure:"withstats"`
	} `mapstructure:"sfu"`
	WebRTC        WebRTCConfig `mapstructure:"webrtc"`
	Router        RouterConfig `mapstructure:"Router"`
	Turn          TurnConfig   `mapstructure:"turn"`
	BufferFactory *buffer.Factory
	TurnAuth      func(username string, realm string, srcAddr net.Addr) ([]byte, bool)
}

/*
packetFactory はRTPパケット用のバイトバッファのプールです。
パケット処理時のメモリアロケーションを削減し、GCの負荷を軽減します。
各バッファは1460バイト（標準的なMTUサイズ）で初期化されます。
*/
var (
	packetFactory *sync.Pool
)

/*
SFU はSFUインスタンスを表すメイン構造体です。
複数のセッションを管理し、各セッションは複数のピア接続を含むことができます。
- webrtc: ピア接続作成用の設定テンプレート
- turn: オプションのTURNサーバー
- sessions: セッションIDをキーとしたセッションマップ
- datachannels: 新規ピアに自動的にネゴシエートされるデータチャネルのリスト
- withStats: 統計収集が有効かどうかのフラグ

【スレッドセーフティ】
sync.RWMutexを埋め込むことで、sessionsマップへの同時アクセスを保護しています。
*/
type SFU struct {
	sync.RWMutex
	webrtc       WebRTCTransportConfig
	turn         *turn.Server
	sessions     map[string]Session
	datachannels []*Datachannel
	withStats    bool
}

/*
NewWebRTCTransportConfig は設定をパースし、ピア接続作成に使用可能なWebRTCTransportConfigを返します。

【処理フロー】
1. SettingEngineの初期化とMediaEngineコピーの無効化（パフォーマンス最適化）
2. ICEポート設定（SinglePortまたはPortRange）
3. ICEサーバーの設定（IceLiteまたは通常のICEサーバー）
4. バッファファクトリーの設定
5. SDPセマンティクスの選択
6. タイムアウトの設定
7. NAT 1:1 IPとmDNSの設定
8. 統計収集の初期化（有効な場合）

【ICEポート設定の戦略】
- SinglePort: すべてのピア接続で1つのUDPポートを共有（ファイアウォールフレンドリー）
- PortRange: 各ピア接続に個別のポートを割り当て（スケーラビリティ向上）
- TURNとポート範囲の共存: TURNが有効でPortRangeが未設定の場合、デフォルト範囲を使用

【戻り値】
完全に初期化されたWebRTCTransportConfig。すべてのピア接続で再利用可能です。
*/
func NewWebRTCTransportConfig(c Config) WebRTCTransportConfig {
	se := webrtc.SettingEngine{}
	se.DisableMediaEngineCopy(true)

	if c.WebRTC.ICESinglePort != 0 {
		Logger.Info("Listen on ", "single-port", c.WebRTC.ICESinglePort)
		udpListener, err := net.ListenUDP("udp", &net.UDPAddr{
			IP:   net.IP{0, 0, 0, 0},
			Port: c.WebRTC.ICESinglePort,
		})
		if err != nil {
			panic(err)
		}
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpListener))
	} else {
		var icePortStart, icePortEnd uint16

		if c.Turn.Enabled && len(c.Turn.PortRange) == 0 {
			icePortStart = sfuMinPort
			icePortEnd = sfuMaxPort
		} else if len(c.WebRTC.ICEPortRange) == 2 {
			icePortStart = c.WebRTC.ICEPortRange[0]
			icePortEnd = c.WebRTC.ICEPortRange[1]
		}
		if icePortStart != 0 || icePortEnd != 0 {
			if err := se.SetEphemeralUDPPortRange(icePortStart, icePortEnd); err != nil {
				panic(err)
			}
		}
	}

	var iceServers []webrtc.ICEServer
	if c.WebRTC.Candidates.IceLite {
		se.SetLite(c.WebRTC.Candidates.IceLite)
	} else {
		for _, iceServer := range c.WebRTC.ICEServers {
			s := webrtc.ICEServer{
				URLs:       iceServer.URLs,
				Username:   iceServer.Username,
				Credential: iceServer.Credential,
			}
			iceServers = append(iceServers, s)
		}
	}

	se.BufferFactory = c.BufferFactory.GetOrNew

	sdpSemantics := webrtc.SDPSemanticsUnifiedPlan
	switch c.WebRTC.SDPSemantics {
	case "unified-plan-with-fallback":
		sdpSemantics = webrtc.SDPSemanticsUnifiedPlanWithFallback
	case "plan-b":
		sdpSemantics = webrtc.SDPSemanticsPlanB
	}

	if c.WebRTC.Timeouts.ICEDisconnectedTimeout == 0 &&
		c.WebRTC.Timeouts.ICEFailedTimeout == 0 &&
		c.WebRTC.Timeouts.ICEKeepaliveInterval == 0 {
		Logger.Info("No webrtc timeouts found in config, using default ones")
	} else {
		se.SetICETimeouts(
			time.Duration(c.WebRTC.Timeouts.ICEDisconnectedTimeout)*time.Second,
			time.Duration(c.WebRTC.Timeouts.ICEFailedTimeout)*time.Second,
			time.Duration(c.WebRTC.Timeouts.ICEKeepaliveInterval)*time.Second,
		)
	}

	w := WebRTCTransportConfig{
		Configuration: webrtc.Configuration{
			ICEServers:   iceServers,
			SDPSemantics: sdpSemantics,
		},
		Setting:       se,
		Router:        c.Router,
		BufferFactory: c.BufferFactory,
	}

	if len(c.WebRTC.Candidates.NAT1To1IPs) > 0 {
		w.Setting.SetNAT1To1IPs(c.WebRTC.Candidates.NAT1To1IPs, webrtc.ICECandidateTypeHost)
	}

	if !c.WebRTC.MDNS {
		w.Setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}

	if c.SFU.WithStats {
		w.Router.WithStats = true
		stats.InitStats()
	}

	return w
}

/*
init はパッケージ初期化時に実行され、packetFactoryを初期化します。
sync.Poolを使用することで、頻繁なメモリアロケーションとGCを回避します。
バッファサイズ1460は、イーサネットのMTU（1500バイト）からヘッダーを引いた
標準的なRTPパケットサイズに対応しています。
*/
func init() {
	// Init packet factory
	packetFactory = &sync.Pool{
		New: func() interface{} {
			b := make([]byte, 1460)
			return &b
		},
	}
}

/*
NewSFU は新しいSFUインスタンスを作成します。

【初期化プロセス】
1. 乱数ジェネレーターのシード設定（暗号学的にはない）
2. バラストメモリの割り当て（GC最適化）
3. BufferFactoryの初期化（未提供の場合）
4. WebRTC Transport設定の構築
5. セッションマップの初期化
6. オプションのTURNサーバーの起動

【バラストメモリについて】
バラストは大きなメモリブロックを割り当てることでGCのヒューリスティックを調整します。
これにより、GCの実行頻度が減少し、低レイテンシーが要求される環境でのパフォーマンスが向上します。
runtime.KeepAlive()により、バラストがGCされないことを保証します。

【TURNサーバー】
Config.Turn.Enabledがtrueの場合、統合TURNサーバーが起動します。
これにより、厳密なファイアウォール環境下でもピア接続が確立できます。

【パラメータ】
c: SFUの設定。すべての必要な設定が含まれている必要があります。

【戻り値】
完全に初期化され、使用可能なSFUインスタンス。
*/
func NewSFU(c Config) *SFU {
	// Init random seed
	rand.Seed(time.Now().UnixNano())
	// Init ballast
	ballast := make([]byte, c.SFU.Ballast*1024*1024)

	if c.BufferFactory == nil {
		c.BufferFactory = buffer.NewBufferFactory(c.Router.MaxPacketTrack, Logger)
	}

	w := NewWebRTCTransportConfig(c)

	sfu := &SFU{
		webrtc:    w,
		sessions:  make(map[string]Session),
		withStats: w.Router.WithStats,
	}

	if c.Turn.Enabled {
		ts, err := InitTurnServer(c.Turn, c.TurnAuth)
		if err != nil {
			Logger.Error(err, "Could not init turn server err")
			os.Exit(1)
		}
		sfu.turn = ts
	}

	runtime.KeepAlive(ballast)
	return sfu
}

/*
newSession は新しいSessionLocalインスタンスを作成し、SFUに登録します。

【処理内容】
1. セッションの作成（データチャネルとWebRTC設定を渡す）
2. クローズハンドラーの設定（セッションマップからの削除と統計のデクリメント）
3. セッションマップへの追加（スレッドセーフ）
4. 統計カウンターのインクリメント（有効な場合）

【セッションのライフサイクル】
- 作成: 最初のピアが参加時に自動的に作成
- クローズ: 最後のピアが退出時に自動的にクローズ
- クリーンアップ: OnCloseコールバックでSFUのマップから自動削除

【スレッドセーフティ】
LockとUnlockを使用して、sessionsマップへの同時書き込みを保護します。

【パラメータ】
id: セッションの一意識別子

【戻り値】
作成されたセッション（Session interface）
*/
func (s *SFU) newSession(id string) Session {
	session := NewSession(id, s.datachannels, s.webrtc).(*SessionLocal)

	session.OnClose(func() {
		s.Lock()
		delete(s.sessions, id)
		s.Unlock()

		if s.withStats {
			stats.Sessions.Dec()
		}
	})

	s.Lock()
	s.sessions[id] = session
	s.Unlock()

	if s.withStats {
		stats.Sessions.Inc()
	}

	return session
}

/*
getSession はセッションIDでセッションを検索します。

【スレッドセーフティ】
RLockとRUnlockを使用して、読み取り専用アクセスを保護します。
複数のゴルーチンが同時に読み取り可能です。

【パラメータ】
id: 検索するセッションのID

【戻り値】
見つかったセッション、または存在しない場合はnil
*/
func (s *SFU) getSession(id string) Session {
	s.RLock()
	defer s.RUnlock()
	return s.sessions[id]
}

/*
GetSession はセッションを取得し、存在しない場合は新規作成します。

【動作】
この関数は遅延セッション作成を実装しています。
セッションが存在しない場合、自動的に新しいセッションを作成します。
これにより、明示的なセッション作成APIが不要になります。

【パラメータ】
sid: セッションID

【戻り値】
- Session: 取得または作成されたセッション
- WebRTCTransportConfig: ピア接続作成用の設定
*/
func (s *SFU) GetSession(sid string) (Session, WebRTCTransportConfig) {
	session := s.getSession(sid)
	if session == nil {
		session = s.newSession(sid)
	}
	return session, s.webrtc
}

/*
NewDatachannel は新しいデータチャネル定義を作成し、SFUに登録します。

【用途】
このデータチャネルは、以降にセッションに参加するすべてのピアに対して
自動的にネゴシエートされます。APIチャネルやカスタムシグナリングチャネルなどに使用します。

【データチャネルミドルウェア】
返されたDatachannelオブジェクトに対してUse()メソッドでミドルウェアを追加でき、
メッセージ処理のカスタマイズが可能です。

【パラメータ】
label: データチャネルのラベル（一意であるべき）

【戻り値】
作成されたDatachannelオブジェクト。ミドルウェアやハンドラーを設定可能です。
*/
func (s *SFU) NewDatachannel(label string) *Datachannel {
	dc := &Datachannel{Label: label}
	s.datachannels = append(s.datachannels, dc)
	return dc
}

/*
GetSessions は現在アクティブなすべてのセッションを返します。

【用途】
- 管理UIでの表示
- 統計情報の収集
- デバッグとモニタリング

【スレッドセーフティ】
RLockを使用して、セッションマップの読み取りを保護します。
返されるスライスは新しく割り当てられたコピーであり、
呼び出し側が安全に使用できます。

【戻り値】
すべてのアクティブセッションのスライス
*/
func (s *SFU) GetSessions() []Session {
	s.RLock()
	defer s.RUnlock()
	sessions := make([]Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}
