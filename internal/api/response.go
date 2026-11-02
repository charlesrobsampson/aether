package api

import (
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// outboundFromResponse converts a satellite SDK response into Aether's
// canonical outbound envelope. Keeping the conversion in one place guarantees
// the two wire shapes stay in lockstep (plan §3.3 and §3.11).
func outboundFromResponse(resp sdk.Response) model.Outbound {
	outbound := model.Outbound{
		Recipients: resp.Recipients,
		Text:       resp.Text,
		Satellite:  resp.Satellite,
		ThreadID:   resp.ThreadID,
	}
	if resp.Session != nil {
		outbound.Session = &model.SessionAction{
			Action:     resp.Session.Action,
			TTLSeconds: resp.Session.TTLSeconds,
		}
	}
	return outbound
}
