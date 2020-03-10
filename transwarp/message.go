/*
	Copyright 2020 NetFoundry, Inc.

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

package transwarp

import "github.com/netfoundry/ziti-fabric/xgress"

func MarshalPayload(p *xgress.Payload, mtu int) ([][]byte, error) {
	return nil, nil
}

func UnmarshalPayload(data [][]byte) (*xgress.Payload, error) {
	return nil, nil
}

func MarshalAcknowledgement(p *xgress.Acknowledgement, mtu int) ([][]byte, error) {
	return nil, nil
}

func UnmarshalAcknowledgement(data [][]byte) (*xgress.Acknowledgement, error) {
	return nil, nil
}