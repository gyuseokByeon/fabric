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
	lastAck               int64
	newlyReceivedAcks     chan *Acknowledgement
	receivedAckHwm        int32
	windowsSize           uint32
	rxBufferSize          uint32
	txBufferSize          uint32
	accumulator           uint32
	closeNotify           chan struct{}
	closed                concurrenz.AtomicBoolean
	blockedByLocalWindow  bool
	blockedByRemoteWindow bool

	config struct {
		retransmitAge uint16
		idleAckAfter  int64
	}
}

type payloadAge struct {
	payload *Payload
	age     int64
}

func (self *payloadAge) markSent() {
	atomic.StoreInt64(&self.age, info.NowInMilliseconds())
}

func (self *payloadAge) getAge() int64 {
	return atomic.LoadInt64(&self.age)
}

func NewLinkSendBuffer(x *Xgress) *LinkSendBuffer {
	buffer := &LinkSendBuffer{
		x:                 x,
		buffer:            make(map[int32]*payloadAge),
		newlyBuffered:     make(chan *payloadAge, x.Options.TxQueueSize),
		lastAck:           info.NowInMilliseconds(),
		newlyReceivedAcks: make(chan *Acknowledgement),
		receivedAckHwm:    -1,
		windowsSize:       256 * 1024,
		closeNotify:       make(chan struct{}),
	}

	buffer.config.retransmitAge = 2000
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

	if buffer.windowsSize < buffer.txBufferSize {
		blocked = true
		if !buffer.blockedByRemoteWindow {
			buffer.blockedByRemoteWindow = true
			atomic.AddInt64(&buffersBlockedByRemoteWindow, 1)
		}
	} else if buffer.blockedByRemoteWindow {
		buffer.blockedByRemoteWindow = false
		atomic.AddInt64(&buffersBlockedByRemoteWindow, -1)
	}

	if buffer.windowsSize < buffer.rxBufferSize {
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
		pfxlog.Logger().Infof("blocked=%v win_size=%v tx_buffer_size=%v rx_buffer_size=%v", blocked, buffer.windowsSize, buffer.txBufferSize, buffer.rxBufferSize)
	}

	return blocked
}

func (buffer *LinkSendBuffer) run() {
	log := pfxlog.ContextLogger("s/" + buffer.x.sessionId)
	defer log.Debugf("[%p] exited", buffer)
	log.Debugf("[%p] started", buffer)

	lastDebug := info.NowInMilliseconds()

	var buffered chan *payloadAge

	for {
		rxBufferSizeHistogram.Update(int64(buffer.rxBufferSize))

		if buffer.isBlocked() {
			buffered = nil
		} else {
			buffered = buffer.newlyBuffered
		}

		now := info.NowInMilliseconds()
		if now-lastDebug >= 2000 {
			buffer.debug(now)
			lastDebug = now
		}

		select {
		case ack := <-buffer.newlyReceivedAcks:
			if err := buffer.receiveAcknowledgement(ack); err != nil {
				log.Errorf("unexpected error (%s)", err)
			}
			if err := buffer.retransmit(); err != nil {
				log.Errorf("unexpected error retransmitting (%s)", err)
			}

		case payloadAge := <-buffered:
			buffer.buffer[payloadAge.payload.GetSequence()] = payloadAge
			buffer.rxBufferSize += uint32(len(payloadAge.payload.Data))
			log.Tracef("buffering payload %v with size %v. payload buffer size: %v",
				payloadAge.payload.Sequence, len(payloadAge.payload.Data), buffer.rxBufferSize)

		case <-time.After(time.Duration(buffer.config.idleAckAfter) * time.Millisecond):
			if err := buffer.retransmit(); err != nil {
				log.Errorf("unexpected error retransmitting (%s)", err)
			}

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

func (buffer *LinkSendBuffer) receiveAcknowledgement(ack *Acknowledgement) error {
	log := pfxlog.Logger().WithFields(ack.GetLoggerFields())
	if buffer.x.sessionId == ack.SessionId {
		for _, sequence := range ack.Sequence {
			if sequence > buffer.receivedAckHwm {
				buffer.receivedAckHwm = sequence
			}
			if payloadAge, found := buffer.buffer[sequence]; found {
				delete(buffer.buffer, sequence)
				buffer.rxBufferSize -= uint32(len(payloadAge.payload.Data))
				log.Debugf("removing payload %v with size %v. payload buffer size: %v",
					payloadAge.payload.Sequence, len(payloadAge.payload.Data), buffer.rxBufferSize)
			}
		}
		buffer.txBufferSize = ack.TxBufferSize
		remoteTxBufferSizeHistogram.Update(int64(buffer.txBufferSize))
		if ack.RTT > 0 {
			buffer.config.retransmitAge = uint16(float32(ack.RTT) * 1.2)
			rttHistogram.Update(int64(ack.RTT))
		}
	} else {
		return errors.New("unexpected acknowledgement")
	}
	return nil
}

func (buffer *LinkSendBuffer) retransmit() error {
	if len(buffer.buffer) > 0 {
		log := pfxlog.ContextLogger(fmt.Sprintf("s/" + buffer.x.sessionId))

		now := info.NowInMilliseconds()
		retransmitted := 0
		for _, v := range buffer.buffer {
			if v.payload.GetSequence() < buffer.receivedAckHwm && uint16(now-v.getAge()) > buffer.config.retransmitAge {
				retransmitter.retransmit(v, buffer.x.address)
				retransmitted++
			}
		}

		if retransmitted > 0 {
			log.Infof("retransmitted [%d] payloads, [%d] buffered, rxBufferSize", retransmitted, len(buffer.buffer), buffer.rxBufferSize)
		}
	}
	return nil
}

func (buffer *LinkSendBuffer) debug(now int64) {
	pfxlog.ContextLogger(buffer.x.sessionId).Debugf("buffer=[%d], lastAck=[%d ms.], receivedAckHwm=[%d]",
		len(buffer.buffer), now-buffer.lastAck, buffer.receivedAckHwm)
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

type ackEntry struct {
	Address
	*Acknowledgement
}
