/*
【ファイル概要: nack.go】
NACKキューはパケット損失の追跡と再送要求の生成を管理します。

【主要な役割】
1. 損失パケットの追跡
  - 欠落したシーケンス番号の検出と記録
  - ソート済みリストによる効率的な管理
  - 最大100個の損失パケットを追跡（maxNackCache）

2. NACK要求の生成
  - RTCP NACKパケットの作成
  - NackPairフォーマット（RFC 4585）への変換
  - 連続した損失パケットの圧縮表現

3. 再送回数の制限
  - 各パケットは最大3回まで再送要求（maxNackTimes）
  - 3回失敗後はキーフレーム要求に切り替え
  - kfSN: キーフレーム要求の重複防止

4. 状態管理
  - push: 新しい損失パケットの追加
  - remove: パケット到着時の削除
  - pairs: RTCP NACKパケット生成とカウンタ更新

【NACKパケット圧縮】
NackPairフォーマット:

	PacketID: 損失パケットのベースシーケンス番号
	LostPackets: 16ビットビットマスク（PacketID+1から+16の範囲）

例:

	損失: [100, 101, 103, 105]
	→ NackPair{PacketID: 100, LostPackets: 0b0000000000010101}
	   ビット0=1: SN 101
	   ビット2=1: SN 103
	   ビット4=1: SN 105

【キーフレーム要求】
以下の条件でキーフレームを要求:
- パケットが3回再送要求しても到着しない
- そのSNがkfSNより大きい（重複要求防止）

【データ構造】

	type nack struct {
	    sn     uint32  // 拡張シーケンス番号（cycles含む）
	    nacked uint8   // 再送要求回数
	}

	type nackQueue struct {
	    nacks []nack   // ソート済みの損失パケットリスト
	    kfSN  uint32   // 最後にキーフレームを要求したSN
	}

【ソート順序】
nacksスライスは常にシーケンス番号でソートされています。
これにより:
- 二分探索による高速な検索（sort.Search）
- 連続した損失パケットの効率的な圧縮
- O(log n)の挿入・削除時間
*/
package buffer

import (
	"sort"

	"github.com/pion/rtcp"
)

const maxNackTimes = 3   // Max number of times a packet will be NACKed
const maxNackCache = 100 // Max NACK sn the sfu will keep reference

type nack struct {
	sn     uint32
	nacked uint8
}

type nackQueue struct {
	nacks []nack
	kfSN  uint32
}

func newNACKQueue() *nackQueue {
	return &nackQueue{
		nacks: make([]nack, 0, maxNackCache+1),
	}
}

func (n *nackQueue) remove(extSN uint32) {
	i := sort.Search(len(n.nacks), func(i int) bool { return n.nacks[i].sn >= extSN })
	if i >= len(n.nacks) || n.nacks[i].sn != extSN {
		return
	}
	copy(n.nacks[i:], n.nacks[i+1:])
	n.nacks = n.nacks[:len(n.nacks)-1]
}

func (n *nackQueue) push(extSN uint32) {
	i := sort.Search(len(n.nacks), func(i int) bool { return n.nacks[i].sn >= extSN })
	if i < len(n.nacks) && n.nacks[i].sn == extSN {
		return
	}

	nck := nack{
		sn:     extSN,
		nacked: 0,
	}
	if i == len(n.nacks) {
		n.nacks = append(n.nacks, nck)
	} else {
		n.nacks = append(n.nacks[:i+1], n.nacks[i:]...)
		n.nacks[i] = nck
	}

	if len(n.nacks) >= maxNackCache {
		copy(n.nacks, n.nacks[1:])
	}
}

func (n *nackQueue) pairs(headSN uint32) ([]rtcp.NackPair, bool) {
	if len(n.nacks) == 0 {
		return nil, false
	}
	i := 0
	askKF := false
	var np rtcp.NackPair
	var nps []rtcp.NackPair
	for _, nck := range n.nacks {
		if nck.nacked >= maxNackTimes {
			if nck.sn > n.kfSN {
				n.kfSN = nck.sn
				askKF = true
			}
			continue
		}
		if nck.sn >= headSN-2 {
			n.nacks[i] = nck
			i++
			continue
		}
		n.nacks[i] = nack{
			sn:     nck.sn,
			nacked: nck.nacked + 1,
		}
		i++
		if np.PacketID == 0 || uint16(nck.sn) > np.PacketID+16 {
			if np.PacketID != 0 {
				nps = append(nps, np)
			}
			np.PacketID = uint16(nck.sn)
			np.LostPackets = 0
			continue
		}
		np.LostPackets |= 1 << (uint16(nck.sn) - np.PacketID - 1)
	}
	if np.PacketID != 0 {
		nps = append(nps, np)
	}
	n.nacks = n.nacks[:i]
	return nps, askKF
}
