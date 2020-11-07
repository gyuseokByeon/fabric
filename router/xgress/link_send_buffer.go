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
	"fmt"
	"github.com/biogo/store/llrb"
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/foundation/util/concurrenz"
	"github.com/openziti/foundation/util/info"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

type PayloadBufferForwarder interface {
	ForwardPayload(srcAddr Address, payload *Payload) error
	ForwardAcknowledgement(srcAddr Address, acknowledgement *Acknowledgement) error
}

type LinkSendBuffer struct {
	x                     *Xgress
	buffer                map[int32]*payloadAge
	newlyBuffered         chan *payloadAge
	newlyReceivedAcks     chan *Acknowledgement
	windowsSize           uint32
	linkSendBufferSize    uint32
	linkRecvBufferSize    uint32
	accumulator           uint32
	successfulAcks        uint32
	duplicateAcks         uint32
	retransmits           uint32
	closeNotify           chan struct{}
	closed                concurrenz.AtomicBoolean
	blockedByLocalWindow  bool
	blockedByRemoteWindow bool
	retransmitAge         uint32
	lastRetransmitTime    int64

	config struct {
		idleAckAfter int64
	}
}

type payloadAge struct {
	payload    *Payload
	age        int64
	retxQueued int32
}

func (self *payloadAge) markSent() {
	atomic.StoreInt64(&self.age, info.NowInMilliseconds())
}

func (self *payloadAge) getAge() int64 {
	return atomic.LoadInt64(&self.age)
}

func (self *payloadAge) markQueued() {
	atomic.AddInt32(&self.retxQueued, 1)
}

func (self *payloadAge) markAcked() {
	atomic.AddInt32(&self.retxQueued, 1)
}

func (self *payloadAge) dequeued() {
	atomic.AddInt32(&self.retxQueued, -1)
}

func (self *payloadAge) isAcked() bool {
	return atomic.LoadInt32(&self.retxQueued) > 1
}

func (self *payloadAge) isRetransmittable() bool {
	return atomic.LoadInt32(&self.retxQueued) == 0
}

func NewLinkSendBuffer(x *Xgress) *LinkSendBuffer {
	buffer := &LinkSendBuffer{
		x:                 x,
		buffer:            make(map[int32]*payloadAge),
		newlyBuffered:     make(chan *payloadAge, x.Options.TxQueueSize),
		newlyReceivedAcks: make(chan *Acknowledgement),
		windowsSize:       x.Options.TxPortalStartSize,
		closeNotify:       make(chan struct{}),
	}

	buffer.retransmitAge = 2000
	buffer.config.idleAckAfter = 5000

	go buffer.run()
	return buffer
}

func (buffer *LinkSendBuffer) BufferPayload(payload *Payload) (func(), error) {
	if buffer.x.sessionId != payload.GetSessionId() {
		return nil, errors.Errorf("bad payload. should have session %v, but had %v", buffer.x.sessionId, payload.GetSessionId())
	}

	payloadAge := &payloadAge{payload: payload, age: math.MaxInt64}
	select {
	case buffer.newlyBuffered <- payloadAge:
		pfxlog.ContextLogger("s/"+payload.GetSessionId()).Debugf("buffered [%d]", payload.GetSequence())
		return payloadAge.markSent, nil
	case <-buffer.closeNotify:
		return nil, errors.Errorf("payload buffer closed")
	}
}

func (buffer *LinkSendBuffer) ReceiveAcknowledgement(ack *Acknowledgement) {
	log := pfxlog.Logger().WithFields(ack.GetLoggerFields())
	log.Debug("ack received")
	select {
	case buffer.newlyReceivedAcks <- ack:
		log.Debug("ack processed")
	case <-buffer.closeNotify:
		log.Error("payload buffer closed")
	}
}

func (buffer *LinkSendBuffer) Close() {
	logrus.Debugf("[%p] closing", buffer)
	if buffer.closed.CompareAndSwap(false, true) {
		close(buffer.closeNotify)
	}
}

func (buffer *LinkSendBuffer) isBlocked() bool {
	blocked := false

	if buffer.windowsSize < buffer.linkRecvBufferSize {
		blocked = true
		if !buffer.blockedByRemoteWindow {
			buffer.blockedByRemoteWindow = true
			atomic.AddInt64(&buffersBlockedByRemoteWindow, 1)
		}
	} else if buffer.blockedByRemoteWindow {
		buffer.blockedByRemoteWindow = false
		atomic.AddInt64(&buffersBlockedByRemoteWindow, -1)
	}

	if buffer.windowsSize < buffer.linkSendBufferSize {
		blocked = true
		if !buffer.blockedByLocalWindow {
			buffer.blockedByLocalWindow = true
			atomic.AddInt64(&buffersBlockedByLocalWindow, 1)
		}
	} else if buffer.blockedByLocalWindow {
		buffer.blockedByLocalWindow = false
		atomic.AddInt64(&buffersBlockedByLocalWindow, -1)
	}

	if blocked {
		pfxlog.Logger().Debugf("blocked=%v win_size=%v tx_buffer_size=%v rx_buffer_size=%v", blocked, buffer.windowsSize, buffer.linkRecvBufferSize, buffer.linkSendBufferSize)
	}

	return blocked
}

func (buffer *LinkSendBuffer) run() {
	log := pfxlog.ContextLogger("s/" + buffer.x.sessionId)
	defer log.Debugf("[%p] exited", buffer)
	log.Debugf("[%p] started", buffer)

	var buffered chan *payloadAge

	retransmitTicker := time.NewTicker(100 * time.Millisecond)

	for {
		txBufferSizeHistogram.Update(int64(buffer.linkSendBufferSize))
		txWindowSize.Update(int64(buffer.windowsSize))

		if buffer.isBlocked() {
			buffered = nil
		} else {
			buffered = buffer.newlyBuffered
		}

		select {
		case ack := <-buffer.newlyReceivedAcks:
			buffer.receiveAcknowledgement(ack)
			buffer.retransmit()

		case payloadAge := <-buffered:
			buffer.buffer[payloadAge.payload.GetSequence()] = payloadAge
			buffer.linkSendBufferSize += uint32(len(payloadAge.payload.Data))
			log.Tracef("buffering payload %v with size %v. payload buffer size: %v",
				payloadAge.payload.Sequence, len(payloadAge.payload.Data), buffer.linkSendBufferSize)

		case <-retransmitTicker.C:
			buffer.retransmit()

		case <-buffer.closeNotify:
			if buffer.blockedByLocalWindow {
				atomic.AddInt64(&buffersBlockedByLocalWindow, -1)
			}
			if buffer.blockedByRemoteWindow {
				atomic.AddInt64(&buffersBlockedByRemoteWindow, -1)
			}
			return
		}
	}
}

func (buffer *LinkSendBuffer) receiveAcknowledgement(ack *Acknowledgement) {
	log := pfxlog.Logger().WithFields(ack.GetLoggerFields())
	if buffer.x.sessionId != ack.SessionId {
		log.Errorf("unexpected acknowledgement. session=%v, but ack is for session=%v", buffer.x.sessionId, ack.Sequence)
		return
	}

	for _, sequence := range ack.Sequence {
		if payloadAge, found := buffer.buffer[sequence]; found {
			payloadAge.markAcked()

			payloadSize := uint32(len(payloadAge.payload.Data))
			buffer.accumulator += payloadSize
			buffer.successfulAcks++
			delete(buffer.buffer, sequence)
			buffer.linkSendBufferSize -= payloadSize
			log.Debugf("removing payload %v with size %v. payload buffer size: %v",
				payloadAge.payload.Sequence, len(payloadAge.payload.Data), buffer.linkSendBufferSize)

			if buffer.successfulAcks >= buffer.x.Options.TxPortalIncreaseThresh {
				buffer.successfulAcks = 0
				delta := uint32(float64(buffer.accumulator) * buffer.x.Options.TxPortalIncreaseScale)
				buffer.windowsSize += delta
				if buffer.windowsSize > buffer.x.Options.TxPortalMaxSize {
					buffer.windowsSize = buffer.x.Options.TxPortalMaxSize
				}
			}
		} else { // duplicate ack
			duplicateAcksMeter.Mark(1)
			buffer.duplicateAcks++
			if buffer.duplicateAcks >= buffer.x.Options.TxPortalDupAckThresh {
				buffer.duplicateAcks = 0
				buffer.scale(buffer.x.Options.TxPortalDupAckScale)
			}
		}
	}

	buffer.linkRecvBufferSize = ack.RecvBufferSize
	remoteRecvBufferSizeHistogram.Update(int64(buffer.linkRecvBufferSize))
	if ack.RTT > 0 {
		buffer.retransmitAge = uint32(float64(ack.RTT)*buffer.x.Options.RetxScale) + buffer.x.Options.RetxAddMs
		rttHistogram.Update(int64(ack.RTT))
	}
}

func (buffer *LinkSendBuffer) retransmit() {
	now := info.NowInMilliseconds()
	if len(buffer.buffer) > 0 && (now-buffer.lastRetransmitTime) > 64 {
		log := pfxlog.ContextLogger(fmt.Sprintf("s/" + buffer.x.sessionId))

		retransmitted := 0
		for _, v := range buffer.buffer {
			if v.isRetransmittable() && uint32(now-v.getAge()) >= buffer.retransmitAge {
				v.markQueued()
				retransmitter.retransmit(v, buffer.x.address)
				retransmitted++
				buffer.retransmits++
				if buffer.retransmits >= buffer.x.Options.TxPortalRetxThresh {
					buffer.retransmits = 0
					buffer.scale(buffer.x.Options.TxPortalRetxScale)
				}
			}
		}

		if retransmitted > 0 {
			log.Infof("retransmitted [%d] payloads, [%d] buffered, linkSendBufferSize: %d", retransmitted, len(buffer.buffer), buffer.linkSendBufferSize)
		}
		buffer.lastRetransmitTime = now
	}
}

func (buffer *LinkSendBuffer) scale(factor float64) {
	buffer.windowsSize = uint32(float64(buffer.windowsSize) * factor)
	if factor > 1 {
		if buffer.windowsSize > buffer.x.Options.TxPortalMaxSize {
			buffer.windowsSize = buffer.x.Options.TxPortalMaxSize
		}
	} else if buffer.windowsSize < buffer.x.Options.TxPortalMinSize {
		buffer.windowsSize = buffer.x.Options.TxPortalMinSize

	}
}

type retransmit struct {
	*payloadAge
	session string
	Address
}

func (r *retransmit) Compare(comparable llrb.Comparable) int {
	if other, ok := comparable.(*retransmit); ok {
		if result := int(r.age - other.age); result != 0 {
			return result
		}
		if result := int(r.payload.Sequence - other.payload.Sequence); result != 0 {
			return result
		}
		return strings.Compare(r.session, other.session)
	}
	pfxlog.Logger().Errorf("*retransmit was compared to %v, which should not happen", reflect.TypeOf(comparable))
	return -1
}
