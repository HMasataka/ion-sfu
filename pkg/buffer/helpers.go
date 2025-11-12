/*
【ファイル概要: helpers.go】
ヘルパー関数とユーティリティ型を提供します。

【主要な役割】
1. atomicBool型
  - boolのスレッドセーフな実装
  - atomic操作によるロックフリーアクセス
  - set()/get()メソッドで簡単に使用可能

2. VP8ペイロード解析
  - VP8 RTPペイロード記述子のパース
  - Temporal Layer情報の抽出
  - Picture ID、TL0PICIDX、TIDの取得
  - キーフレーム検出

3. H.264キーフレーム検出
  - H.264 NALUタイプの判定
  - IDR（Instantaneous Decoder Refresh）フレームの検出
  - STAP-A/B、MTAP、FU-A/Bの処理

【VP8ペイロード記述子】
RFC 7741で定義されたVP8 RTPペイロード形式:

基本フォーマット:

	0 1 2 3 4 5 6 7
	+-+-+-+-+-+-+-+-+
	|X|R|N|S|R| PID | (REQUIRED)
	+-+-+-+-+-+-+-+-+

拡張フォーマット（X=1の場合）:

	|I|L|T|K| RSV   | (OPTIONAL)
	+-+-+-+-+-+-+-+-+
	|M| PictureID   | (OPTIONAL, I=1)
	+-+-+-+-+-+-+-+-+
	|   TL0PICIDX   | (OPTIONAL, L=1)
	+-+-+-+-+-+-+-+-+
	|TID|Y| KEYIDX  | (OPTIONAL, T=1 or K=1)
	+-+-+-+-+-+-+-+-+

フィールド説明:
- X: 拡張ビット（1=拡張フィールドあり）
- S: Start of VP8 partition（1=パーティション開始）
- PictureID: フレーム識別子（7または15ビット）
- TL0PICIDX: Temporal layer 0のインデックス
- TID: Temporal layer ID（0-3）

【H.264 NALUタイプ】
NALUタイプ（下位5ビット）:
- 1-23: 単一NALUパケット
- 5: IDRスライス（キーフレーム）
- 24: STAP-A（単一時刻集約パケット）
- 25: STAP-B
- 26: MTAP16
- 27: MTAP24
- 28: FU-A（分割ユニット）
- 29: FU-B

【タイムスタンプ処理】
RTPタイムスタンプは32ビットでラップアラウンドします:
- IsTimestampWrapAround: ラップアラウンド検出
- IsLaterTimestamp: ラップアラウンドを考慮した時刻比較

ラップアラウンド例:

	timestamp1 = 0xFFFFFFFF (最大値)
	timestamp2 = 0x00000001 (ラップアラウンド後)
	→ timestamp2 > timestamp1 (時間的に後)
*/
package buffer

import (
	"encoding/binary"
	"errors"
	"sync/atomic"
)

var (
	errShortPacket = errors.New("packet is not large enough")
	errNilPacket   = errors.New("invalid nil packet")
)

type atomicBool int32

func (a *atomicBool) set(value bool) {
	var i int32
	if value {
		i = 1
	}
	atomic.StoreInt32((*int32)(a), i)
}

func (a *atomicBool) get() bool {
	return atomic.LoadInt32((*int32)(a)) != 0
}

// VP8 is a helper to get temporal data from VP8 packet header
/*
	VP8 Payload Descriptor
			0 1 2 3 4 5 6 7                      0 1 2 3 4 5 6 7
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
			|X|R|N|S|R| PID | (REQUIRED)        |X|R|N|S|R| PID | (REQUIRED)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		X:  |I|L|T|K| RSV   | (OPTIONAL)   X:   |I|L|T|K| RSV   | (OPTIONAL)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		I:  |M| PictureID   | (OPTIONAL)   I:   |M| PictureID   | (OPTIONAL)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		L:  |   TL0PICIDX   | (OPTIONAL)        |   PictureID   |
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		T/K:|TID|Y| KEYIDX  | (OPTIONAL)   L:   |   TL0PICIDX   | (OPTIONAL)
			+-+-+-+-+-+-+-+-+                   +-+-+-+-+-+-+-+-+
		T/K:|TID|Y| KEYIDX  | (OPTIONAL)
			+-+-+-+-+-+-+-+-+
*/
type VP8 struct {
	TemporalSupported bool
	// Optional Header
	PictureID uint16 /* 8 or 16 bits, picture ID */
	PicIDIdx  int
	MBit      bool
	TL0PICIDX uint8 /* 8 bits temporal level zero index */
	TlzIdx    int

	// Optional Header If either of the T or K bits are set to 1,
	// the TID/Y/KEYIDX extension field MUST be present.
	TID uint8 /* 2 bits temporal layer idx*/
	// IsKeyFrame is a helper to detect if current packet is a keyframe
	IsKeyFrame bool
}

// Unmarshal parses the passed byte slice and stores the result in the VP8 this method is called upon
func (p *VP8) Unmarshal(payload []byte) error {
	if payload == nil {
		return errNilPacket
	}

	payloadLen := len(payload)

	if payloadLen < 1 {
		return errShortPacket
	}

	idx := 0
	S := payload[idx]&0x10 > 0
	// Check for extended bit control
	if payload[idx]&0x80 > 0 {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check if T is present, if not, no temporal layer is available
		p.TemporalSupported = payload[idx]&0x20 > 0
		K := payload[idx]&0x10 > 0
		L := payload[idx]&0x40 > 0
		// Check for PictureID
		if payload[idx]&0x80 > 0 {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			p.PicIDIdx = idx
			pid := payload[idx] & 0x7f
			// Check if m is 1, then Picture ID is 15 bits
			if payload[idx]&0x80 > 0 {
				idx++
				if payloadLen < idx+1 {
					return errShortPacket
				}
				p.MBit = true
				p.PictureID = binary.BigEndian.Uint16([]byte{pid, payload[idx]})
			} else {
				p.PictureID = uint16(pid)
			}
		}
		// Check if TL0PICIDX is present
		if L {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			p.TlzIdx = idx

			if int(idx) >= payloadLen {
				return errShortPacket
			}
			p.TL0PICIDX = payload[idx]
		}
		if p.TemporalSupported || K {
			idx++
			if payloadLen < idx+1 {
				return errShortPacket
			}
			p.TID = (payload[idx] & 0xc0) >> 6
		}
		if idx >= payloadLen {
			return errShortPacket
		}
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		p.IsKeyFrame = payload[idx]&0x01 == 0 && S
	} else {
		idx++
		if payloadLen < idx+1 {
			return errShortPacket
		}
		// Check is packet is a keyframe by looking at P bit in vp8 payload
		p.IsKeyFrame = payload[idx]&0x01 == 0 && S
	}
	return nil
}

// isH264Keyframe detects if h264 payload is a keyframe
// this code was taken from https://github.com/jech/galene/blob/codecs/rtpconn/rtpreader.go#L45
// all credits belongs to Juliusz Chroboczek @jech and the awesome Galene SFU
func isH264Keyframe(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	nalu := payload[0] & 0x1F
	if nalu == 0 {
		// reserved
		return false
	} else if nalu <= 23 {
		// simple NALU
		return nalu == 5
	} else if nalu == 24 || nalu == 25 || nalu == 26 || nalu == 27 {
		// STAP-A, STAP-B, MTAP16 or MTAP24
		i := 1
		if nalu == 25 || nalu == 26 || nalu == 27 {
			// skip DON
			i += 2
		}
		for i < len(payload) {
			if i+2 > len(payload) {
				return false
			}
			length := uint16(payload[i])<<8 |
				uint16(payload[i+1])
			i += 2
			if i+int(length) > len(payload) {
				return false
			}
			offset := 0
			if nalu == 26 {
				offset = 3
			} else if nalu == 27 {
				offset = 4
			}
			if offset >= int(length) {
				return false
			}
			n := payload[i+offset] & 0x1F
			if n == 7 {
				return true
			} else if n >= 24 {
				// is this legal?
				Logger.V(0).Info("Non-simple NALU within a STAP")
			}
			i += int(length)
		}
		if i == len(payload) {
			return false
		}
		return false
	} else if nalu == 28 || nalu == 29 {
		// FU-A or FU-B
		if len(payload) < 2 {
			return false
		}
		if (payload[1] & 0x80) == 0 {
			// not a starting fragment
			return false
		}
		return payload[1]&0x1F == 7
	}
	return false
}
