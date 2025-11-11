/*
【ファイル概要: relay.go】
リレー設定のオプション関数。

SFU間のメディアリレー時の動作をカスタマイズするための
関数オプションパターンの実装です。
*/
package sfu

func RelayWithFanOutDataChannels() func(r *relayPeer) {
	return func(r *relayPeer) {
		r.relayFanOutDataChannels = true
	}
}

func RelayWithSenderReports() func(r *relayPeer) {
	return func(r *relayPeer) {
		r.withSRReports = true
	}
}
