/*
【ファイル概要: simulcast.go】
Simulcast関連の設定と補助構造体。

Simulcast（複数品質ストリーム）の設定と、
レイヤー切り替え時の状態管理を提供します。
VP8のテンポラルスケーラビリティもサポートします。
*/
package sfu

import "time"

const (
	quarterResolution = "q"
	halfResolution    = "h"
	fullResolution    = "f"
)

type SimulcastConfig struct {
	BestQualityFirst    bool `mapstructure:"bestqualityfirst"`
	EnableTemporalLayer bool `mapstructure:"enabletemporallayer"`
}

type simulcastTrackHelpers struct {
	switchDelay       time.Time
	temporalSupported bool
	temporalEnabled   bool
	lTSCalc           int64

	// VP8Helper temporal helpers
	pRefPicID  uint16
	refPicID   uint16
	lPicID     uint16
	pRefTlZIdx uint8
	refTlZIdx  uint8
	lTlZIdx    uint8
	refSN      uint16
}
