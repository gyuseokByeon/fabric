/*
	Copyright NetFoundry, Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package handler_link

import (
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/fabric/router/forwarder"
	"github.com/openziti/fabric/router/xgress"
	"github.com/openziti/fabric/router/xlink"
	"github.com/openziti/foundation/channel2"
)

type payloadHandler struct {
	link      xlink.Xlink
	ctrl      xgress.CtrlChannel
	forwarder *forwarder.Forwarder
}

func newPayloadHandler(link xlink.Xlink, ctrl xgress.CtrlChannel, forwarder *forwarder.Forwarder) *payloadHandler {
	return &payloadHandler{link: link, ctrl: ctrl, forwarder: forwarder}
}

func (self *payloadHandler) ContentType() int32 {
	return xgress.ContentTypePayloadType
}

func (self *payloadHandler) HandleReceive(msg *channel2.Message, ch channel2.Channel) {
	log := pfxlog.ContextLogger(ch.Label())

	payload, err := xgress.UnmarshallPayload(msg)
	if err == nil {
		if err := self.forwarder.ForwardPayload(xgress.Address(self.link.Id().Token), payload); err != nil {
			log.Debugf("unable to forward (%v)", err)
		}
		if payload.IsSessionEndFlagSet() {
			self.forwarder.EndSession(payload.GetSessionId())
		}
	} else {
		log.Errorf("unexpected error (%v)", err)
	}
}
