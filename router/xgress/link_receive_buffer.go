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

package xgress

import (
	"github.com/emirpasic/gods/trees/btree"
	"github.com/emirpasic/gods/utils"
	"github.com/michaelquigley/pfxlog"
	"sync/atomic"
)

type LinkReceiveBuffer struct {
	tree               *btree.Tree
	sequence           int32
	size               uint32
	lastBufferSizeSent uint32
}

func NewLinkReceiveBuffer() *LinkReceiveBuffer {
	return &LinkReceiveBuffer{
		tree:     btree.NewWith(10240, utils.Int32Comparator),
		sequence: -1,
	}
}

func (buffer *LinkReceiveBuffer) Size() uint32 {
	return atomic.LoadUint32(&buffer.size)
}

func (buffer *LinkReceiveBuffer) ReceiveUnordered(payload *Payload) {
	if payload.GetSequence() > buffer.sequence {
		treeSize := buffer.tree.Size()
		buffer.tree.Put(payload.GetSequence(), payload)
		if buffer.tree.Size() > treeSize {
			payloadSize := len(payload.Data)
			size := atomic.AddUint32(&buffer.size, uint32(payloadSize))
			pfxlog.Logger().Tracef("Payload %v of size %v added to transmit buffer. New size: %v", payload.Sequence, payloadSize, size)
			localTxBufferSizeHistogram.Update(int64(size))
		}
	}
}

func (buffer *LinkReceiveBuffer) NextReadyPayload() *Payload {
	if buffer.tree.LeftKey() != nil {
		if nextSequence := buffer.tree.LeftKey().(int32); nextSequence == buffer.sequence+1 {
			return buffer.tree.LeftValue().(*Payload)
		}
	}
	return nil
}

func (buffer *LinkReceiveBuffer) RemoveReadyPayload() {
	nextSequence := buffer.tree.LeftKey().(int32)
	buffer.tree.Remove(nextSequence)
	buffer.sequence = nextSequence
}

func (buffer *LinkReceiveBuffer) getLastBufferSizeSent() uint32 {
	return atomic.LoadUint32(&buffer.lastBufferSizeSent)
}
