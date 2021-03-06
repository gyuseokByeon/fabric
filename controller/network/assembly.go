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

package network

import (
	"github.com/golang/protobuf/proto"
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/fabric/pb/ctrl_pb"
	"github.com/openziti/foundation/channel2"
	"github.com/openziti/foundation/util/info"
)

func (network *Network) assemble() {
	log := pfxlog.Logger()

	if network.Routers.connectedCount() > 1 {
		log.Debugf("assembling with [%d] routers", network.Routers.connectedCount())

		missingLinks, err := network.linkController.missingLinks(network.Routers.allConnected())
		if err == nil {
			for _, missingLink := range missingLinks {
				network.linkController.add(missingLink)

				dial := &ctrl_pb.Dial{
					Id:      missingLink.Id.Token,
					Address: missingLink.Dst.AdvertisedListener,
				}
				bytes, err := proto.Marshal(dial)
				if err == nil {
					msg := channel2.NewMessage(int32(ctrl_pb.ContentType_DialType), bytes)
					missingLink.Src.Control.Send(msg)

				} else {
					log.Errorf("unexpected error (%s)", err)
				}
			}

		} else {
			log.WithField("err", err).Error("missing link enumeration failed")
		}
	}
}

func (network *Network) clean() {
	log := pfxlog.Logger()

	failedLinks := network.linkController.linksInMode(Failed)

	now := info.NowInMilliseconds()
	lRemove := make(map[string]*Link)
	for _, l := range failedLinks {
		if now-l.CurrentState().Timestamp >= 30000 {
			lRemove[l.Id.Token] = l
		}
	}
	for _, lr := range lRemove {
		log.Infof("removing [l/%s]", lr.Id.Token)
		network.linkController.remove(lr)
	}
}
