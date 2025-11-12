/*
【ファイル概要: errors.go】
Buffer関連のエラー定義を提供します。

【エラー一覧】
1. errPacketNotFound
  - パケットがキャッシュに見つからない
  - 原因: 要求されたシーケンス番号が古すぎる、または未到着
  - 発生箇所: Bucket.GetPacket()

2. errBufferTooSmall
  - 提供されたバッファが小さすぎる
  - 原因: パケットサイズより小さいバッファが渡された
  - 発生箇所: Bucket.GetPacket()、Buffer.Read()

3. errPacketTooOld
  - 受信したパケットが古すぎる
  - 原因: 現在のウィンドウ範囲外のシーケンス番号
  - 発生箇所: Bucket.set()
  - 詳細: headSN - sn >= maxSteps+1 の場合

4. errRTXPacket
  - パケットが既に受信済み
  - 原因: 重複パケットまたは再送パケット（RTX）
  - 発生箇所: Bucket.set()
  - 処理: 無視される（エラーとして扱われない場合もある）

【エラー処理パターン】
Buffer.Write() → Bucket.AddPacket() → Bucket.set()

	→ errRTXPacket: 無視（正常）
	→ errPacketTooOld: 無視（ログのみ）

Receiver.RetransmitPackets() → Buffer.GetPacket()

	→ errPacketNotFound: スキップして次のパケット
	→ errBufferTooSmall: エラーログ

【エラーの重要度】
- errRTXPacket: 情報（重複は正常動作）
- errPacketTooOld: 警告（ネットワーク遅延の可能性）
- errPacketNotFound: 警告（再送失敗）
- errBufferTooSmall: エラー（実装バグの可能性）
*/
package buffer

import "errors"

var (
	errPacketNotFound = errors.New("packet not found in cache")
	errBufferTooSmall = errors.New("buffer too small")
	errPacketTooOld   = errors.New("received packet too old")
	errRTXPacket      = errors.New("packet already received")
)
