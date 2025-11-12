/*
【ファイル概要: factory.go】
FactoryはBufferとRTCPReaderの集中管理とプーリングを提供します。

【主要な役割】
1. オブジェクトの集中管理
  - SSRC（同期ソース識別子）ごとにBufferを管理
  - SSRCごとにRTCPReaderを管理
  - マップによる高速なオブジェクト検索

2. メモリプーリング
  - videoPool: ビデオ用バッファのプール
  - audioPool: 音声用バッファのプール
  - sync.Poolによるメモリ再利用でGC負荷を軽減

3. ライフサイクル管理
  - GetOrNew: 既存オブジェクトの取得または新規作成
  - OnCloseコールバックによる自動クリーンアップ
  - Bufferが閉じられると自動的にマップから削除

4. ペアアクセス
  - GetBufferPair: BufferとRTCPReaderを同時取得
  - RTPとRTCPの処理を簡素化

【メモリプール設計】
  - videoPool: trackingPackets * maxPktSize バイト
    例: 500パケット × 1500バイト = 750KB
  - audioPool: maxPktSize * 25 バイト
    例: 1500バイト × 25 = 37.5KB

音声は小さいパケットが多いため、ビデオより小さいバッファを使用。

【スレッドセーフティ】
- sync.RWMutexによる排他制御
- 読み取り操作は並行実行可能（RLock）
- 書き込み操作は排他的（Lock）

【使用パターン】

 1. Factoryを作成
    factory := NewBufferFactory(500, logger)

 2. WebRTC RTPReceiverごとにBufferを取得
    buf := factory.GetOrNew(packetio.RTPBufferPacket, ssrc)

 3. WebRTC RTCPReceiverごとにRTCPReaderを取得
    reader := factory.GetOrNew(packetio.RTCPBufferPacket, ssrc)
*/
package buffer

import (
	"io"
	"sync"

	"github.com/go-logr/logr"
	"github.com/pion/transport/packetio"
)

type Factory struct {
	sync.RWMutex
	videoPool   *sync.Pool
	audioPool   *sync.Pool
	rtpBuffers  map[uint32]*Buffer
	rtcpReaders map[uint32]*RTCPReader
	logger      logr.Logger
}

func NewBufferFactory(trackingPackets int, logger logr.Logger) *Factory {
	// Enable package wide logging for non-method functions.
	// If logger is empty - use default Logger.
	// Logger is a public variable in buffer package.
	if logger == (logr.Logger{}) {
		logger = Logger
	} else {
		Logger = logger
	}

	return &Factory{
		videoPool: &sync.Pool{
			New: func() interface{} {
				b := make([]byte, trackingPackets*maxPktSize)
				return &b
			},
		},
		audioPool: &sync.Pool{
			New: func() interface{} {
				b := make([]byte, maxPktSize*25)
				return &b
			},
		},
		rtpBuffers:  make(map[uint32]*Buffer),
		rtcpReaders: make(map[uint32]*RTCPReader),
		logger:      logger,
	}
}

func (f *Factory) GetOrNew(packetType packetio.BufferPacketType, ssrc uint32) io.ReadWriteCloser {
	f.Lock()
	defer f.Unlock()
	switch packetType {
	case packetio.RTCPBufferPacket:
		if reader, ok := f.rtcpReaders[ssrc]; ok {
			return reader
		}
		reader := NewRTCPReader(ssrc)
		f.rtcpReaders[ssrc] = reader
		reader.OnClose(func() {
			f.Lock()
			delete(f.rtcpReaders, ssrc)
			f.Unlock()
		})
		return reader
	case packetio.RTPBufferPacket:
		if reader, ok := f.rtpBuffers[ssrc]; ok {
			return reader
		}
		buffer := NewBuffer(ssrc, f.videoPool, f.audioPool, f.logger)
		f.rtpBuffers[ssrc] = buffer
		buffer.OnClose(func() {
			f.Lock()
			delete(f.rtpBuffers, ssrc)
			f.Unlock()
		})
		return buffer
	}
	return nil
}

func (f *Factory) GetBufferPair(ssrc uint32) (*Buffer, *RTCPReader) {
	f.RLock()
	defer f.RUnlock()
	return f.rtpBuffers[ssrc], f.rtcpReaders[ssrc]
}

func (f *Factory) GetBuffer(ssrc uint32) *Buffer {
	f.RLock()
	defer f.RUnlock()
	return f.rtpBuffers[ssrc]
}

func (f *Factory) GetRTCPReader(ssrc uint32) *RTCPReader {
	f.RLock()
	defer f.RUnlock()
	return f.rtcpReaders[ssrc]
}
