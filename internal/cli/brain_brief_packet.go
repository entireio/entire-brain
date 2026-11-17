package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// brainBriefPacketFormat names the brief's two render formats where the
// profile output records them. The experimental compact_v1/v2/v3 serializers
// were deleted (COR-2317): no CLI flag or env ever selected them — only tests
// did — so the dispatch below is the whole surface.
type brainBriefPacketFormat string

const (
	brainBriefPacketText       brainBriefPacketFormat = "text"
	brainBriefPacketLegacyJSON brainBriefPacketFormat = "json"
)

func (opts brainBriefOptions) resolvedPacketFormat() brainBriefPacketFormat {
	if opts.json {
		return brainBriefPacketLegacyJSON
	}
	return brainBriefPacketText
}

func emitBrainBriefPacket(cmd *cobra.Command, report brainBriefReport, format brainBriefPacketFormat) error {
	switch format {
	case brainBriefPacketText:
		return emitBrainBriefReport(cmd, report, false)
	case brainBriefPacketLegacyJSON:
		return emitBrainBriefReport(cmd, report, true)
	default:
		return fmt.Errorf("unsupported brain brief packet format: %q", format)
	}
}
