package twcc

import (
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"sync"

	"github.com/gammazero/deque"
	"github.com/pion/rtcp"
)

// 定数定義：TWCC(Transport Wide Congestion Control)フィードバックパケットのヘッダフィールドオフセット
const (
	// baseSequenceNumberOffset: ペイロード内でベースシーケンス番号が格納される位置（8バイト目）
	baseSequenceNumberOffset = 8
	// packetStatusCountOffset: ペイロード内でパケットステータスカウントが格納される位置（10バイト目）
	packetStatusCountOffset = 10
	// referenceTimeOffset: ペイロード内で参照時刻が格納される位置（12バイト目）
	referenceTimeOffset = 12

	// tccReportDelta: 通常のフィードバック送信間隔（100ミリ秒 = 1e8ナノ秒）
	// この時間経過するか、一定数のパケットが溜まるとTWCCフィードバックを生成する
	tccReportDelta = 1e8
	// tccReportDeltaAfterMark: RTPマーカビット設定後のフィードバック送信間隔（50ミリ秒 = 50e6ナノ秒）
	// フレーム終了時にはこちらの短い間隔でフィードバックを送信し、より迅速に帯域幅変更に対応する
	tccReportDeltaAfterMark = 50e6
)

// rtpExtInfo: RTPパケットの拡張ヘッダから取得された情報を保持する構造体
// TransportWideシーケンス番号と受信時刻のペアを管理し、TWCC計算の基礎データとなる
type rtpExtInfo struct {
	// ExtTSN: 拡張シーケンス番号（Extended Transport Wide Sequence Number）
	// 16ビットのシーケンス番号を複数サイクル分含む32ビット値
	// サイクルカウントとシーケンス番号を組み合わせることで、パケットロスを正確に検出できる
	ExtTSN uint32
	// Timestamp: パケット受信時刻（マイクロ秒単位）
	// 各パケットの到着時間を記録し、到着間隔（デルタ）の計算に使用される
	Timestamp int64
}

// Responder: Transport Wide Congestion Control(TWCC)フィードバック生成用の応答器
// RTPパケットのTransport Wide拡張ヘッダからシーケンス番号を抽出し、
// RTCP Feedback Message(Transport Specific Feedback Type)を生成して送信する
// 仕様に従う: https://tools.ietf.org/html/draft-holmer-rmcat-transport-wide-cc-extensions-01
//
// TWCCは受信側がパケットの受信状況（受信/ロス）とパケット到着間隔のデルタ時刻を
// 送信側にフィードバックすることで、より正確な帯域幅推定を可能にする機構である。
// 従来のREMB(Receiver Estimated Maximum Bitrate)よりも詳細な情報をフィードバックできる。
type Responder struct {
	// ゴルーチンセーフ性を提供するためのミューテックス
	sync.Mutex

	// extInfo: 受信したRTPパケットの拡張シーケンス番号と時刻情報の履歴
	// フィードバック送信時に処理されクリアされる
	extInfo []rtpExtInfo
	// lastReport: 前回のTWCCフィードバック送信時刻（ナノ秒単位）
	// フィードバック送信間隔を判定するために使用される
	lastReport int64
	// cycles: シーケンス番号のサイクルカウント
	// 16ビットのシーケンス番号がオーバーフローする度にインクリメントされる
	// 拡張シーケンス番号（32ビット）を計算するために使用される
	cycles uint32
	// lastExtSN: 前回処理した拡張シーケンス番号
	// フィードバックに含めるパケットの範囲を決定するために使用される
	// 一度処理したパケット情報は次回以降も追加しないようにするために必要
	lastExtSN uint32
	// pktCtn: フィードバックパケットカウント（0-255でラップアラウンド）
	// 受信側が複数回のフィードバックを区別するためのシーケンス番号として
	// TWCC RTCPパケットに含められる
	pktCtn uint8
	// lastSn: 前回受信したシーケンス番号（16ビット）
	// サイクルカウント計算時にオーバーフロー検出に使用される
	lastSn uint16
	// lastExtInfo: 前回処理した拡張シーケンス番号（16ビット、lastExtSNの下位16ビット）
	// パフォーマンス最適化のため保持されている（現在使用されていない可能性あり）
	lastExtInfo uint16
	// mSSRC: メディアソースSSRC（Synchronization Source identifier）
	// TWCCフィードバックで参照する、フィードバック対象のメディアストリームのSSRC
	mSSRC uint32
	// sSSRC: 送信側（Sender）SSRC
	// TWCCフィードバックパケット自体の送信元を識別するための値
	// 受信側で生成される（通常はランダム値）
	sSSRC uint32

	// len: ペイロードバッファ（payload）の現在の有効データ長（バイト単位）
	// TWCCフィードバックのヘッダ部とパケットステータス情報を保持する
	len uint16
	// deltaLen: デルタバッファ（deltas）の現在の有効データ長（バイト単位）
	// パケット到着間隔の差分情報を保持する
	deltaLen uint16
	// payload: TWCC RTCPパケットのペイロード格納バッファ（100バイト）
	// ヘッダ情報とパケットステータスチャンク情報を格納する
	payload [100]byte
	// deltas: パケット到着間隔（タイムスタンプデルタ）格納バッファ（200バイト）
	// 各パケット間の時間差を1バイト（小さなデルタ）または2バイト（大きなデルタ）で格納
	deltas [200]byte
	// chunk: パケットステータスシンボルチャンク一時構築用バッファ
	// ビット操作による複数パケットの状態情報を効率的に詰め込む際に使用される
	chunk uint16

	// onFeedback: TWCCフィードバック生成時のコールバック関数
	// 生成されたRTCPフィードバックパケットを外部に通知するためのハンドラー
	onFeedback func(packet rtcp.RawPacket)
}

// NewTransportWideCCResponder: Transport Wide Congestion Control応答器の生成
// 指定されたメディアソースSSRCを対象とする新しいResponderインスタンスを作成する。
// 送信側SSRC(sSSRC)はランダムに生成される。
//
// 引数:
//   - ssrc: フィードバック対象のメディアストリームのSSRC（mSSRCとして使用される）
//
// 返り値: 初期化されたResponderインスタンスへのポインタ
func NewTransportWideCCResponder(ssrc uint32) *Responder {
	return &Responder{
		// extInfo: 初期容量101でスライスを確保
		// 通常20-100パケット程度をバッチ処理するため、少し多めに確保することでメモリ効率化
		extInfo: make([]rtpExtInfo, 0, 101),
		// sSSRC: 送信側SSRCをランダムに生成（通常は受信側が生成する値）
		sSSRC: rand.Uint32(),
		// mSSRC: 指定されたメディアソースSSRC（フィードバック対象）
		mSSRC: ssrc,
	}
}

// Push: RTPパケットのTransport Wide拡張ヘッダから取得したシーケンス番号を登録
// 受信したRTPパケットの情報をバッファに追加し、フィードバック送信条件を判定する。
// 条件が満たされるとTWCCフィードバックパケットを生成・送信する。
//
// 処理フロー:
// 1. シーケンス番号がオーバーフローしたかを検出し、サイクルカウントを更新
// 2. パケット情報をextInfoスライスに追加
// 3. フィードバック送信条件をチェック:
//   - 最低20パケット以上溜まっている
//   - mSSRCが設定されている（有効なフィードバック対象がある）
//   - 以下のいずれかを満たす:
//     a) 前回送信からtccReportDelta(100ms)以上経過
//     b) 100パケット以上溜まった
//     c) RTPマーカビットが立っており、tccReportDeltaAfterMark(50ms)以上経過
//
// 引数:
//   - sn: Transport Wide Sequence Number（16ビット）
//   - timeNS: パケット受信時刻（ナノ秒単位、通常はtime.Now().UnixNano()）
//   - marker: RTPヘッダのマーカビット（フレーム終了を示す）
func (t *Responder) Push(sn uint16, timeNS int64, marker bool) {
	t.Lock()
	defer t.Unlock()

	// シーケンス番号のオーバーフロー検出とサイクルカウント更新
	// 前回のシーケンス番号が0xf000以上（上位4ビットが1）で、
	// 今回のシーケンス番号が0x0fff以下（オーバーフロー直後）の場合、
	// サイクルが1進んだと判断
	if sn < 0x0fff && (t.lastSn&0xffff) > 0xf000 {
		t.cycles += 1 << 16
	}

	// パケット情報を拡張シーケンス番号（ExtTSN）と受信時刻で記録
	// ExtTSN = (サイクルカウント | シーケンス番号)により、32ビットの一意識別子を生成
	// 受信時刻はナノ秒からマイクロ秒に変換（1e3で除算）
	t.extInfo = append(t.extInfo, rtpExtInfo{
		ExtTSN:    t.cycles | uint32(sn),
		Timestamp: timeNS / 1e3,
	})

	// 初回パケット受信時に基準時刻を設定
	if t.lastReport == 0 {
		t.lastReport = timeNS
	}

	t.lastSn = sn

	// 前回のフィードバック送信から現在までの経過時間を計算
	delta := timeNS - t.lastReport

	// フィードバック送信判定
	// 3つの条件をすべて満たす場合にTWCCフィードバックを生成・送信する:
	// 1. len(t.extInfo) > 20: 最低限十分なパケット情報が溜まっている
	// 2. t.mSSRC != 0: 有効なメディアソースが指定されている
	// 3. 時間またはパケット数の条件:
	//    - delta >= tccReportDelta (100ms以上経過)
	//    - len(t.extInfo) > 100 (100パケット以上)
	//    - marker && delta >= tccReportDeltaAfterMark (フレーム終了で50ms以上経過)
	if len(t.extInfo) > 20 && t.mSSRC != 0 &&
		(delta >= tccReportDelta || len(t.extInfo) > 100 || (marker && delta >= tccReportDeltaAfterMark)) {
		if pkt := t.buildTransportCCPacket(); pkt != nil {
			t.onFeedback(pkt)
		}
		t.lastReport = timeNS
	}
}

// OnFeedback: TWCCフィードバック生成時のコールバック関数を設定
// Push()内で生成されたTWCCフィードバックRTCPパケットを通知するハンドラーを登録する。
// このコールバックは生成されたRTCPパケットを外部（通常はRTCP送信ロジック）に
// 渡すために使用される。
//
// 引数:
//   - f: RTCPフィードバックパケットを受け取るコールバック関数
func (t *Responder) OnFeedback(f func(p rtcp.RawPacket)) {
	t.onFeedback = f
}

// buildTransportCCPacket: TWCCフィードバックRTCPパケットを構築
// 蓄積されたパケット受信情報からTWCC形式のRTCPパケットを生成する。
// このメソッドは以下の処理を実行:
// 1. 受信情報の検証と整列
// 2. ロストパケットのギャップを検出して挿入
// 3. パケットステータスと到着時刻デルタを効率的に圧縮
// 4. RFC仕様に従ったRTCPフィードバックパケットを生成
//
// 返り値: 構築されたRTCPパケット（rtcp.RawPacket）、またはnilが返されることもある
func (t *Responder) buildTransportCCPacket() rtcp.RawPacket {
	// 受信情報がない場合は処理しない
	if len(t.extInfo) == 0 {
		return nil
	}

	// 拡張シーケンス番号でソート
	// 受信順序ではなく、シーケンス番号の大小順で整列させることで、
	// シーケンス番号の連続性を判定しやすくする
	sort.Slice(t.extInfo, func(i, j int) bool {
		return t.extInfo[i].ExtTSN < t.extInfo[j].ExtTSN
	})

	// 最終的なフィードバック対象パケット情報を構築
	// 受信したパケット情報に加えて、ロストパケットを示すエントリ（Timestamp=0）を挿入
	// スライス容量は元のサイズの1.2倍（ロストパケット分を考慮）
	tccPkts := make([]rtpExtInfo, 0, int(float64(len(t.extInfo))*1.2))
	for _, tccExtInfo := range t.extInfo {
		// 既に処理したシーケンス番号より小さい場合はスキップ
		// （重複フィードバックを避けるため）
		if tccExtInfo.ExtTSN < t.lastExtSN {
			continue
		}

		// 前回処理したシーケンス番号とのギャップを検出
		// ギャップ内のシーケンス番号に対して、Timestamp=0のロストパケットエントリを作成
		if t.lastExtSN != 0 {
			for j := t.lastExtSN + 1; j < tccExtInfo.ExtTSN; j++ {
				tccPkts = append(tccPkts, rtpExtInfo{ExtTSN: j})
			}
		}

		// 現在のシーケンス番号を処理済みとしてマーク
		t.lastExtSN = tccExtInfo.ExtTSN
		// 受信したパケットエントリを追加
		tccPkts = append(tccPkts, tccExtInfo)
	}

	// extInfoスライスをクリアして次のフィードバック処理に備える
	t.extInfo = t.extInfo[:0]

	// パケットステータスの圧縮エンコーディング処理用の変数
	// firstRecv: 最初の受信パケットを処理したかどうかのフラグ
	firstRecv := false
	// same: 連続するパケットのステータスが同じかどうかを示す
	// Run-Length Encoding（RLE）の使用判定に用いられる
	same := true
	// timestamp: 参照時刻からの相対時刻基準値（マイクロ秒単位）
	// 各パケットの到着時刻デルタを計算するために使用される
	timestamp := int64(0)
	// lastStatus: 前のパケットのステータス
	// ステータスの変化を検出するために保持
	lastStatus := rtcp.TypeTCCPacketReceivedWithoutDelta
	// maxStatus: 現在処理しているパケットグループ内での最大ステータス値
	// グループ全体のステータスを決定するために使用
	maxStatus := rtcp.TypeTCCPacketNotReceived

	// statusList: パケットステータスの一時キュー
	// Run-Length EncodingまたはStatus Symbol Chunkで処理する前に
	// 複数パケットのステータスを一度に保持する
	var statusList deque.Deque
	statusList.SetMinCapacity(3)

	// パケット情報の処理ループ
	// tccPkts内の各パケットについて、ステータス情報とタイムスタンプデルタを抽出し、
	// 効率的に圧縮してペイロードに追加する
	for _, stat := range tccPkts {
		// デフォルトステータスはパケット未受信
		status := rtcp.TypeTCCPacketNotReceived

		// Timestamp != 0はパケット受信を示す（Timestamp=0はロストパケット）
		if stat.Timestamp != 0 {
			var delta int64

			// 初回受信パケット処理時にヘッダ情報とタイムスタンプ基準値を設定
			if !firstRecv {
				firstRecv = true
				// 参照時刻を64ミリ秒（64e3マイクロ秒）の粒度で正規化
				// TWCC仕様では参照時刻は64ms単位で記録される
				refTime := stat.Timestamp / 64e3
				// 正規化された参照時刻に基づいて相対タイムスタンプの基準値を設定
				timestamp = refTime * 64e3
				// TWCCパケットヘッダを書き込む
				// 第1引数: ベースシーケンス番号（tccPktsの最初のパケットのシーケンス番号）
				// 第2引数: パケット数
				// 第3引数: 正規化された参照時刻（32ビット、単位は64ms）
				t.writeHeader(
					uint16(tccPkts[0].ExtTSN),
					uint16(len(tccPkts)),
					uint32(refTime),
				)
				// フィードバックパケットカウントをインクリメント
				t.pktCtn++
			}

			// 参照時刻からのデルタ時刻を計算
			// 各パケットの到着時刻と基準時刻の差を250マイクロ秒の単位で計算
			// （1 TCC time unit = 250マイクロ秒）
			delta = (stat.Timestamp - timestamp) / 250

			// デルタ値の範囲チェック
			// デルタが0-255の範囲外の場合は大きなデルタ（2バイト）で表現
			if delta < 0 || delta > 255 {
				// 大きなデルタ（Large Delta）: 2バイト符号付き整数で表現
				status = rtcp.TypeTCCPacketReceivedLargeDelta
				rDelta := int16(delta)
				// int16への変換でオーバーフロー/アンダーフロー判定
				// 範囲外の値は最大/最小値でクリップ
				if int64(rDelta) != delta {
					if rDelta > 0 {
						rDelta = math.MaxInt16
					} else {
						rDelta = math.MinInt16
					}
				}
				// 大きなデルタ値をdeltas配列に書き込む
				t.writeDelta(status, uint16(rDelta))
			} else {
				// 小さなデルタ（Small Delta）: 1バイト符号なし整数で表現
				status = rtcp.TypeTCCPacketReceivedSmallDelta
				// 小さなデルタ値をdeltas配列に書き込む
				t.writeDelta(status, uint16(delta))
			}

			// 次のパケットのタイムスタンプ基準値を更新
			timestamp = stat.Timestamp
		}

		// ステータスの圧縮エンコーディング判定ロジック
		// Run-Length Encoding（RLE）とStatus Symbol Chunkの使い分けにより、
		// パケットステータス情報を効率的に圧縮する

		// RLE可能性の判定：
		// - 同じステータスが連続している（same == true）
		// - かつ、ステータスが変わった（status != lastStatus）
		// - かつ、前のステータスが「パケット受信なし」状態ではない
		if same && status != lastStatus && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta {
			// Run-Length Chunkで効率的に表現できるか判定（最大7個まで）
			if statusList.Len() > 7 {
				// 7個以上のステータスが同じため、Run-Length Chunkで出力
				t.writeRunLengthChunk(lastStatus, uint16(statusList.Len()))
				// ステータスバッファをクリア
				statusList.Clear()
				// 状態をリセット
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				same = true
			} else {
				// 少数のステータス（7個以下）が変わったため、
				// Status Symbol Chunkで表現する（複数ステータスを複数ビットで圧縮）
				same = false
			}
		}

		// 現在のステータスをキューに追加
		statusList.PushBack(status)
		// グループ内での最大ステータスを更新
		if status > maxStatus {
			maxStatus = status
		}
		lastStatus = status

		// Status Symbol Chunk出力条件1：
		// ステータスが異なる場合で、かつ最大ステータスが大きなデルタ（2ビット必要）で、
		// 十分なパケット（7個以上）が溜まった場合
		if !same && maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta && statusList.Len() > 6 {
			// 7個のステータスを2ビット/個でStatus Symbol Chunkに圧縮
			for i := 0; i < 7; i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, statusList.PopFront().(uint16), i)
			}
			// Status Symbol Chunkを出力
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
			// ステータス記録をリセット
			lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
			maxStatus = rtcp.TypeTCCPacketNotReceived
			same = true

			// 残りのステータスについてsame/maxStatusを再評価
			for i := 0; i < statusList.Len(); i++ {
				status = statusList.At(i).(uint16)
				if status > maxStatus {
					maxStatus = status
				}
				// ステータスの変化を検出
				if same && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
					same = false
				}
				lastStatus = status
			}
		} else if !same && statusList.Len() > 13 {
			// Status Symbol Chunk出力条件2：
			// ステータスが異なる場合で、14個以上のステータスが溜まった場合
			// 14個のステータスを1ビット/個でStatus Symbol Chunkに圧縮
			for i := 0; i < 14; i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, statusList.PopFront().(uint16), i)
			}
			// Status Symbol Chunkを出力
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
			// ステータス記録をリセット
			lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
			maxStatus = rtcp.TypeTCCPacketNotReceived
			same = true
		}
	}

	// 残りのステータス情報を処理
	if statusList.Len() > 0 {
		if same {
			// 残りのステータスがすべて同じ場合、Run-Length Chunkで出力
			t.writeRunLengthChunk(lastStatus, uint16(statusList.Len()))
		} else if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta {
			// 残りのステータスに大きなデルタが含まれる場合、2ビットで出力
			for i := 0; i < statusList.Len(); i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit, statusList.PopFront().(uint16), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeTwoBit)
		} else {
			// 残りのステータスが小さなステータスのみの場合、1ビットで出力
			for i := 0; i < statusList.Len(); i++ {
				t.createStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit, statusList.PopFront().(uint16), i)
			}
			t.writeStatusSymbolChunk(rtcp.TypeTCCSymbolSizeOneBit)
		}
	}

	// RTCP パケット全体のサイズを計算
	// 構成: RTCPヘッダ(4バイト) + ペイロード(len) + デルタ(deltaLen)
	pLen := t.len + t.deltaLen + 4
	// RTCPはワード境界（4バイト）でアライメントされる必要があるため、
	// パディングが必要かどうかを判定
	pad := pLen%4 != 0
	var padSize uint8
	// パディング計算：パケットサイズを4の倍数にするために必要なパディングバイト数
	for pLen%4 != 0 {
		padSize++
		pLen++
	}

	// RTCPヘッダの構築
	hdr := rtcp.Header{
		// Padding: パディングが必要な場合はtrue
		Padding: pad,
		// Length: RTCPヘッダ自体を含めた32ビットワード単位のパケット長 - 1
		// RFC 3550仕様に従う
		Length: (pLen / 4) - 1,
		// Count: Transport Specific Feedback Type (TWCC)
		Count: rtcp.FormatTCC,
		// Type: Transport Specific Feedback (205)
		Type: rtcp.TypeTransportSpecificFeedback,
	}
	// ヘッダをマーシャル（バイナリ化）
	hb, _ := hdr.Marshal()

	// RTCPパケット全体を構築
	pkt := make(rtcp.RawPacket, pLen)
	// ヘッダをコピー（先頭4バイト）
	copy(pkt, hb)
	// ペイロード（ヘッダ + パケットステータス情報）をコピー
	copy(pkt[4:], t.payload[:t.len])
	// タイムスタンプデルタ情報をコピー
	copy(pkt[4+t.len:], t.deltas[:t.deltaLen])
	// パディングが必要な場合、最後のバイトにパディング長を設定
	if pad {
		pkt[len(pkt)-1] = padSize
	}

	// デルタ情報バッファをリセット
	// ペイロードバッファは次のフィードバック生成時にはヘッダで上書きされるため、
	// ここではリセット不要だが、deltaLenは必ずリセットが必要
	t.deltaLen = 0

	return pkt
}

// writeHeader: TWCC RTCPフィードバックパケットのヘッダ部を構築
// RFC仕様で定義されるTWCCフィードバックパケットのフォーマット:
//
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                     SSRC of packet sender (32bits)            |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|                      SSRC of media source (32bits)            |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|      base sequence number (16bits)  |  packet status count(16) |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|              reference time (24bits)          | fb pkt cnt(8) |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// 引数:
//   - bSN: ベースシーケンス番号（最初のパケットのシーケンス番号）
//   - packetCount: パケット統計情報に含まれるパケット数
//   - refTime: 参照時刻（64ms単位の正規化された値）
func (t *Responder) writeHeader(bSN, packetCount uint16, refTime uint32) {
	// バイト0-3: 送信者のSSRC
	// このフィードバックパケット自体の送信元を識別する
	binary.BigEndian.PutUint32(t.payload[0:], t.sSSRC)

	// バイト4-7: メディアソースのSSRC
	// フィードバック対象のメディアストリームを識別する
	binary.BigEndian.PutUint32(t.payload[4:], t.mSSRC)

	// バイト8-9: ベースシーケンス番号
	// フィードバックに含まれるパケット統計の最初のシーケンス番号
	binary.BigEndian.PutUint16(t.payload[baseSequenceNumberOffset:], bSN)

	// バイト10-11: パケット統計カウント
	// フィードバックに含まれるパケットの総数（受信/ロスの両方を含む）
	binary.BigEndian.PutUint16(t.payload[packetStatusCountOffset:], packetCount)

	// バイト12-15: 参照時刻とフィードバックパケットカウント
	// 上位24ビット（シフト8）: 参照時刻（64ms単位）
	// 下位8ビット: フィードバックパケットカウント（0-255）
	// 参照時刻は受信側の時計時刻の一部で、送信側の時刻トラッキングに使用される
	binary.BigEndian.PutUint32(t.payload[referenceTimeOffset:], refTime<<8|uint32(t.pktCtn))

	// ペイロードの有効データ長を設定（ヘッダは16バイト）
	t.len = 16
}

// writeRunLengthChunk: Run-Length Encoding形式のパケットステータスチャンクを書き込み
// Run-Length Chunk（RLE）は連続する同じステータスを効率的に表現するフォーマット
// 複数の連続するパケットが同じステータス（受信/ロス）を持つ場合に使用
//
// フォーマット（16ビット）:
//
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|T| S |       Run Length (13bits)|
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// - T (1bit): Type フラグ（Run-Length Chunkの場合は0）
// - S (2bits): Symbol（パケットステータス: 00=ロス, 01=受信(小デルタ), 10=未使用, 11=受信(大デルタ)）
// - Run Length (13bits): 同じステータスの連続パケット数（最大8191）
//
// 引数:
//   - symbol: パケットステータス値（左シフト13ビットで配置）
//   - runLength: 連続するパケット数
func (t *Responder) writeRunLengthChunk(symbol uint16, runLength uint16) {
	// シンボル値を13ビット左シフトしてタイプフラグ（0）と結合
	// symbolはすでに左シフト13の値（2ビット値を13左シフト）
	// runLengthはそのまま結合
	binary.BigEndian.PutUint16(t.payload[t.len:], symbol<<13|runLength)
	// ペイロード長を2バイト増加
	t.len += 2
}

// createStatusSymbolChunk: Status Symbol Chunk形式でパケットステータスを圧縮エンコード
// Status Symbol Chunk（SSC）は複数のパケットステータスを可変長ビットでコンパクトに
// 表現するフォーマット。複数ステータスが異なる場合や、連続するステータスが8個以上の
// 場合に使用される。最大14パケット（1ビット/個）または7パケット（2ビット/個）をまとめられる。
//
// フォーマット（16ビット）:
//
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|T|S| symbol[0] | symbol[1] ... |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// - T (1bit): Type フラグ（Status Symbol Chunkの場合は1）
// - S (1bit): Symbol Size（0=1ビット/シンボル, 1=2ビット/シンボル）
// - symbol list: 複数のパケットステータスを1ビットまたは2ビットで列挙
//
// 引数:
//   - symbolSize: シンボルサイズ(TypeTCCSymbolSizeOneBit=0 または TypeTCCSymbolSizeTwoBit=1)
//   - symbol: 追加するパケットステータス値
//   - i: 現在のシンボル位置（0から開始）
func (t *Responder) createStatusSymbolChunk(symbolSize, symbol uint16, i int) {
	// シンボルビット数を計算：
	// - 1ビットシンボルの場合: symbolSize=0, numOfBits=1
	// - 2ビットシンボルの場合: symbolSize=1, numOfBits=2
	numOfBits := symbolSize + 1
	// setNBitsOfUint16を使用して、指定ビット位置にシンボル値を設定
	// パラメータ：
	// - 第1引数: 対象バッファ（t.chunk）
	// - 第2引数: 設定するビット数（numOfBits）
	// - 第3引数: ビット位置（最初は2ビット目から、タイプフラグとサイズフラグをスキップ）
	// - 第4引数: 設定する値（symbol）
	t.chunk = setNBitsOfUint16(t.chunk, numOfBits, numOfBits*uint16(i)+2, symbol)
}

// writeStatusSymbolChunk: 構築済みStatus Symbol Chunkをペイロードに書き込み
// createStatusSymbolChunkで段階的に構築されたStatus Symbol Chunkを
// ペイロードに確定する。タイプフラグとサイズフラグを設定し、
// バッファに記録する。
//
// 処理:
// 1. タイプフラグ（ビット0）を1に設定（Status Symbol Chunk識別子）
// 2. シンボルサイズフラグ（ビット1）を設定
// 3. 構築済みのt.chunkをペイロードに書き込み
// 4. 次のチャンク構築に備えてバッファをリセット
//
// 引数:
//   - symbolSize: シンボルサイズ(0=1ビット, 1=2ビット)
func (t *Responder) writeStatusSymbolChunk(symbolSize uint16) {
	// タイプフラグ（ビット0）を1に設定
	// この値により、Run-Length Chunkではなく Status Symbol Chunkであることを示す
	t.chunk = setNBitsOfUint16(t.chunk, 1, 0, 1)

	// シンボルサイズフラグ（ビット1）を設定
	// 0=1ビットシンボル（14個まで可能）
	// 1=2ビットシンボル（7個まで可能）
	t.chunk = setNBitsOfUint16(t.chunk, 1, 1, symbolSize)

	// 完成したStatus Symbol Chunkをペイロードに書き込み
	binary.BigEndian.PutUint16(t.payload[t.len:], t.chunk)

	// バッファをリセット（次のチャンク構築に備える）
	t.chunk = 0

	// ペイロード長を2バイト増加
	t.len += 2
}

// writeDelta: パケット到着時刻のデルタ値をデルタバッファに書き込み
// パケット到着間隔（タイムスタンプデルタ）を効率的に格納する。
// 小さなデルタ（0-255マイクロ秒）と大きなデルタ（-32768～+32767マイクロ秒）を
// 異なる形式で保存する。
//
// パケットステータスのデルタ情報フォーマット:
// - 受信状態なし/パケット未受信: デルタなし
// - 受信（小デルタ）: 1バイト符号なし整数（0-255）
// - 受信（大デルタ）: 2バイト符号付き整数（-32768～+32767）
//
// 引数:
//   - deltaType: デルタのタイプ（小デルタまたは大デルタ）
//   - delta: デルタ値（250マイクロ秒単位で正規化されている）
func (t *Responder) writeDelta(deltaType, delta uint16) {
	// 小さなデルタ（1バイト）の場合
	if deltaType == rtcp.TypeTCCPacketReceivedSmallDelta {
		// 1バイトで格納（符号なし値0-255）
		t.deltas[t.deltaLen] = byte(delta)
		t.deltaLen++
		return
	}

	// 大きなデルタ（2バイト）の場合
	// 符号付き値をビッグエンディアンで格納
	binary.BigEndian.PutUint16(t.deltas[t.deltaLen:], delta)
	t.deltaLen += 2
}

// setNBitsOfUint16: uint16型バッファの指定ビット位置にビット列を設定
// ビット単位での効率的なデータ配置を行うためのユーティリティ関数。
// ビットフィールドの操作を抽象化し、TWCC圧縮フォーマットの構築に使用される。
//
// 処理:
// 1. 指定サイズのビットマスクを作成
// 2. 値をマスクでトリムケート（指定ビット幅に切り詰め）
// 3. トリムケートされた値を開始位置にシフト
// 4. 元の値とORで結合（既存ビットに追加）
//
// 例: val=3, size=2, startIndex=0の場合
// - マスク: 11b
// - トリム後: 11b
// - シフト量: 16 - 2 - 0 = 14
// - 結果: 11b << 14 = 1100000000000000b
//
// 引数:
//   - src: 対象となる元のuint16値
//   - size: 設定するビット数
//   - startIndex: ビット開始位置（最上位ビット0から開始）
//   - val: 設定する値
//
// 返り値: 指定されたビット位置に値を設定した新しいuint16値
func setNBitsOfUint16(src, size, startIndex, val uint16) uint16 {
	// ビット位置の妥当性チェック
	// startIndex + size が16を超えると、16ビットの範囲外になるため不正
	if startIndex+size > 16 {
		return 0
	}

	// 値をsizeビット幅に切り詰める
	// (1 << size) - 1でsizeビット分の1が立ったマスクを作成
	// 例: size=2 -> 11b (3), size=8 -> 11111111b (255)
	val &= (1 << size) - 1

	// 切り詰められた値を正しいビット位置にシフト
	// シフト量 = 16 - size - startIndex
	// これにより、startIndexからsize幅分のビット位置に値を配置
	// 最後にsrcとORで既存ビットと結合
	return src | (val << (16 - size - startIndex))
}
