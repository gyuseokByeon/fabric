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

package handler_xgress

import (
	"github.com/golang/protobuf/proto"
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/fabric/pb/ctrl_pb"
	"github.com/openziti/fabric/router/forwarder"
	"github.com/openziti/fabric/router/xgress"
	"github.com/openziti/foundation/channel2"
)

type closeHandler struct {
	ctrl      xgress.CtrlChannel
	forwarder *forwarder.Forwarder
}

func NewCloseHandler(ctrl xgress.CtrlChannel, forwarder *forwarder.Forwarder) *closeHandler {
	return &closeHandler{ctrl: ctrl, forwarder: forwarder}
}

func (txc *closeHandler) HandleXgressClose(x *xgress.Xgress) {
	log := pfxlog.ContextLogger(x.Label())
	log.Debug("running")
	defer log.Debug("complete")

	// Send end of session payload
	log.Debug("sending end of session payload")
	if err := txc.forwarder.ForwardPayload(x.Address(), x.GetEndSession()); err != nil {
		// ok that we couldn't forward close, as that means it was already closed
		log.Debugf("error forwarding end session payload (%s)", err)
	}

	// Notify the forwarder that the session is ending
	log.Debug("removing session from forwarder")
	txc.forwarder.EndSession(x.SessionId())

	// Notify the controller of the xgress fault
	fault := &ctrl_pb.Fault{Id: x.SessionId()}
	if x.Originator() == xgress.Initiator {
		fault.Subject = ctrl_pb.FaultSubject_IngressFault
	} else if x.Originator() == xgress.Terminator {
		fault.Subject = ctrl_pb.FaultSubject_EgressFault
	}
	if body, err := proto.Marshal(fault); err == nil {
		msg := channel2.NewMessage(int32(ctrl_pb.ContentType_FaultType), body)
		log.Debug("notifying controller of fault")
		if err := txc.ctrl.Channel().Send(msg); err != nil {
			log.Errorf("error sending fault (%s)", err)
		}
	} else {
		log.Errorf("error marshalling (%s)", err)
	}
}
