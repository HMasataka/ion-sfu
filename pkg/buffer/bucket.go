/*
【ファイル概要: bucket.go】
Bucketは固定サイズのリングバッファによるRTPパケットストレージを提供します。

【主要な役割】
1. 循環バッファ管理
  - 固定サイズのバイト配列を使用した効率的なパケット保存
  - リングバッファ構造による古いパケットの自動上書き
  - パケットサイズの制限（maxPktSize = 1500バイト）

2. パケットの保存と取得
  - AddPacket: 新しいパケットの追加（順序保証あり/なし）
  - GetPacket: シーケンス番号によるパケット取得
  - 重複パケットの検出と拒否

3. シーケンス番号管理
  - headSN: 最新のシーケンス番号を追跡
  - step: リングバッファ内の現在位置
  - maxSteps: バッファに保存可能な最大パケット数

【データ構造】
バッファフォーマット（各パケット）:

	[0-1]: パケットサイズ（uint16, big endian）
	[2-N]: 実際のRTPパケットデータ

各パケットは maxPktSize（1500バイト）のスロットに保存されます。

【動作原理】
- 最新パケットが到着すると、stepを進めて次のスロットに書き込み
- 古いパケット（順序が乱れた）は適切な位置を計算して保存
- stepがmaxStepsを超えると0に戻る（循環）
- パケット検索時はシーケンス番号から位置を逆算

【パケット配置例】
buf: [pkt0][pkt1][pkt2][pkt3]...[pktN]

	↑step=0

最新: headSN=100

新しいパケット101到着:
buf: [pkt0][pkt1][pkt2][pkt3]...[pktN]

	↑step=1

最新: headSN=101
*/
package buffer

import (
	"encoding/binary"
	"math"
)

const maxPktSize = 1500

type Bucket struct {
	buf []byte
	src *[]byte

	init     bool
	step     int
	headSN   uint16
	maxSteps int
}

func NewBucket(buf *[]byte) *Bucket {
	return &Bucket{
		src:      buf,
		buf:      *buf,
		maxSteps: int(math.Floor(float64(len(*buf))/float64(maxPktSize))) - 1,
	}
}

func (b *Bucket) AddPacket(pkt []byte, sn uint16, latest bool) ([]byte, error) {
	if !b.init {
		b.headSN = sn - 1
		b.init = true
	}
	if !latest {
		return b.set(sn, pkt)
	}
	diff := sn - b.headSN
	b.headSN = sn
	for i := uint16(1); i < diff; i++ {
		b.step++
		if b.step >= b.maxSteps {
			b.step = 0
		}
	}
	return b.push(pkt), nil
}

func (b *Bucket) GetPacket(buf []byte, sn uint16) (i int, err error) {
	p := b.get(sn)
	if p == nil {
		err = errPacketNotFound
		return
	}
	i = len(p)
	if cap(buf) < i {
		err = errBufferTooSmall
		return
	}
	if len(buf) < i {
		buf = buf[:i]
	}
	copy(buf, p)
	return
}

func (b *Bucket) push(pkt []byte) []byte {
	binary.BigEndian.PutUint16(b.buf[b.step*maxPktSize:], uint16(len(pkt)))
	off := b.step*maxPktSize + 2
	copy(b.buf[off:], pkt)
	b.step++
	if b.step > b.maxSteps {
		b.step = 0
	}
	return b.buf[off : off+len(pkt)]
}

func (b *Bucket) get(sn uint16) []byte {
	pos := b.step - int(b.headSN-sn+1)
	if pos < 0 {
		if pos*-1 > b.maxSteps+1 {
			return nil
		}
		pos = b.maxSteps + pos + 1
	}
	off := pos * maxPktSize
	if off > len(b.buf) {
		return nil
	}
	if binary.BigEndian.Uint16(b.buf[off+4:off+6]) != sn {
		return nil
	}
	sz := int(binary.BigEndian.Uint16(b.buf[off : off+2]))
	return b.buf[off+2 : off+2+sz]
}

func (b *Bucket) set(sn uint16, pkt []byte) ([]byte, error) {
	if b.headSN-sn >= uint16(b.maxSteps+1) {
		return nil, errPacketTooOld
	}
	pos := b.step - int(b.headSN-sn+1)
	if pos < 0 {
		pos = b.maxSteps + pos + 1
	}
	off := pos * maxPktSize
	if off > len(b.buf) || off < 0 {
		return nil, errPacketTooOld
	}
	// Do not overwrite if packet exist
	if binary.BigEndian.Uint16(b.buf[off+4:off+6]) == sn {
		return nil, errRTXPacket
	}
	binary.BigEndian.PutUint16(b.buf[off:], uint16(len(pkt)))
	copy(b.buf[off+2:], pkt)
	return b.buf[off+2 : off+2+len(pkt)], nil
}
