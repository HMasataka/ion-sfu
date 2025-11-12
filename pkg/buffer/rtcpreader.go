/*
【ファイル概要: rtcpreader.go】
RTCPReaderは着信RTCPパケットの処理とコールバック通知を提供します。

【主要な役割】
1. RTCPパケットの受信
  - Write()メソッドで着信RTCPパケットを受け取る
  - io.WriteCloserインターフェースの実装
  - WebRTC Interceptorパイプラインとの統合

2. コールバック通知
  - OnPacket: RTCPパケット到着時のコールバック設定
  - atomic.Valueによるスレッドセーフなコールバック管理
  - パケットバイト列をそのまま通知

3. ライフサイクル管理
  - OnClose: クローズ時のコールバック
  - closed フラグによる重複処理防止
  - Factoryによる自動クリーンアップ

【使用パターン】
Router内での使用例:

	rtcpReader.OnPacket(func(bytes []byte) {
	    pkts, err := rtcp.Unmarshal(bytes)
	    for _, pkt := range pkts {
	        switch pkt := pkt.(type) {
	        case *rtcp.SenderReport:
	            // Sender Reportの処理
	            buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
	        }
	    }
	})

【スレッドセーフティ】
- atomic.Valueによるコールバック関数の安全な読み書き
- atomicBoolによる閉鎖状態の管理
- 複数のゴルーチンから安全に呼び出し可能

【WebRTC統合】
pion/webrtcのInterceptorと統合:

	InterceptorRegistry
	  ↓ RTCP packets
	RTCPReader.Write()
	  ↓ callback
	Router RTCP処理
*/
package buffer

import (
	"io"
	"sync/atomic"
)

type RTCPReader struct {
	ssrc     uint32
	closed   atomicBool
	onPacket atomic.Value //func([]byte)
	onClose  func()
}

func NewRTCPReader(ssrc uint32) *RTCPReader {
	return &RTCPReader{ssrc: ssrc}
}

func (r *RTCPReader) Write(p []byte) (n int, err error) {
	if r.closed.get() {
		err = io.EOF
		return
	}
	if f, ok := r.onPacket.Load().(func([]byte)); ok {
		f(p)
	}
	return
}

func (r *RTCPReader) OnClose(fn func()) {
	r.onClose = fn
}

func (r *RTCPReader) Close() error {
	r.closed.set(true)
	r.onClose()
	return nil
}

func (r *RTCPReader) OnPacket(f func([]byte)) {
	r.onPacket.Store(f)
}

func (r *RTCPReader) Read(_ []byte) (n int, err error) { return }
