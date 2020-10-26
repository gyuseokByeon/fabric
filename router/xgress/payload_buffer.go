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
	"github.com/ef-ds/deque"
	"github.com/emirpasic/gods/trees/btree"
	"github.com/emirpasic/gods/utils"
	"github.com/michaelquigley/pfxlog"
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

type PayloadBufferController struct {
	forwarder        PayloadBufferForwarder
	retransmits      *llrb.Tree
	retransmitIngest chan *retransmit
	retransmitSend   chan *retransmit
	acks             *deque.Deque
	ackIngest        chan *ackEntry
	ackSend          chan *ackEntry
	ackTicker        *time.Ticker
}

func NewPayloadBufferController(forwarder PayloadBufferForwarder) *PayloadBufferController {
	ctrl := &PayloadBufferController{
		forwarder:        forwarder,
		retransmits:      &llrb.Tree{},
		retransmitIngest: make(chan *retransmit, 16),
		retransmitSend:   make(chan *retransmit, 1),
		acks:             deque.New(),
		ackIngest:        make(chan *ackEntry, 16),
		ackSend:          make(chan *ackEntry, 1),
		ackTicker:        time.NewTicker(250 * time.Millisecond),
	}
	go ctrl.ackIngester()
	go ctrl.ackSender()
	go ctrl.retransmitIngester()
	go ctrl.retransmitSender()
	return ctrl
}

func (controller *PayloadBufferController) retransmit(p *payloadAge, address Address) {
	controller.retransmitIngest <- &retransmit{
		payloadAge: p,
		Address:    address,
	}
}

func (controller *PayloadBufferController) ack(ack *Acknowledgement, address Address) {
	controller.ackIngest <- &ackEntry{
		Acknowledgement: ack,
		Address:         address,
	}
}

func (controller *PayloadBufferController) retransmitIngester() {
	var next *retransmit
	for {
		if next == nil && controller.retransmits.Count > 1 {
			next = controller.retransmits.Max().(*retransmit)
			controller.retransmits.DeleteMax()
		}

		if next == nil {
			select {
			case retransmit := <-controller.retransmitIngest:
				controller.retransmits.Insert(retransmit)
			}
		} else {
			select {
			case retransmit := <-controller.retransmitIngest:
				controller.retransmits.Insert(retransmit)
			case controller.retransmitSend <- next:
				next = nil
			}
		}
	}
}

func (controller *PayloadBufferController) ackIngester() {
	var next *ackEntry
	for {
		if next == nil {
			if val, _ := controller.acks.PopFront(); val != nil {
				next = val.(*ackEntry)
			}
		}

		if next == nil {
			select {
			case ack := <-controller.ackIngest:
				controller.acks.PushBack(ack)
			}
		} else {
			select {
			case ack := <-controller.ackIngest:
				controller.acks.PushBack(ack)
			case controller.ackSend <- next:
				next = nil
			}
		}
	}
}

func (controller *PayloadBufferController) ackSender() {
	logger := pfxlog.Logger()
	for nextAck := range controller.ackSend {
		if err := controller.forwarder.ForwardAcknowledgement(nextAck.Address, nextAck.Acknowledgement); err != nil {
			logger.WithError(err).Errorf("unexpected error while sending ack from %v", nextAck.Address)
			ackFailures.Mark(1)
		} else {
			acks.Mark(1)
		}
	}
}

func (controller *PayloadBufferController) retransmitSender() {
	logger := pfxlog.Logger()
	for retransmit := range controller.retransmitSend {
		if err := controller.forwarder.ForwardPayload(retransmit.Address, retransmit.payloadAge.payload); err != nil {
			logger.WithError(err).Errorf("unexpected error while retransmitting payload from %v", retransmit.Address)
			retransmissionFailures.Mark(1)
		} else {
			retransmissions.Mark(1)
			retransmit.markSent()
		}
	}
}

type PayloadBuffer struct {
	x                 *Xgress
	buffer            map[int32]*payloadAge
	newlyBuffered     chan *payloadAge
	acked             map[int32]int64
	lastAck           int64
	newlyAcknowledged chan *Payload
	newlyReceivedAcks chan *Acknowledgement
	receivedAckHwm    int32
	controller        *PayloadBufferController
	transmitBuffer    TransmitBuffer
	freeSpace         uint32
	mostRecentRTT     uint16

	config struct {
		retransmitAge uint16
		ackPeriod     int64
		ackCount      int
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

func NewPayloadBuffer(x *Xgress, controller *PayloadBufferController) *PayloadBuffer {
	buffer := &PayloadBuffer{
		x:                 x,
		buffer:            make(map[int32]*payloadAge),
		newlyBuffered:     make(chan *payloadAge),
		acked:             make(map[int32]int64),
		lastAck:           info.NowInMilliseconds(),
		newlyAcknowledged: make(chan *Payload),
		newlyReceivedAcks: make(chan *Acknowledgement),
		receivedAckHwm:    -1,
		controller:        controller,
		transmitBuffer: TransmitBuffer{
			tree:     btree.NewWith(10240, utils.Int32Comparator),
			sequence: -1,
		},
		freeSpace: 64 * 1024,
	}

	buffer.config.retransmitAge = 2000
	buffer.config.ackPeriod = 64
	buffer.config.ackCount = 96
	buffer.config.idleAckAfter = 5000

	go buffer.run()
	return buffer
}

func (buffer *PayloadBuffer) BufferPayload(payload *Payload) (func(), error) {
	defer func() {
		if r := recover(); r != nil {
			pfxlog.ContextLogger("s/" + buffer.x.sessionId).Error("send on closed channel")
		}
	}()

	if buffer.x.sessionId != payload.GetSessionId() {
		return nil, errors.Errorf("bad payload. should have session %v, but had %v", buffer.x.sessionId, payload.GetSessionId())
	}

	payloadAge := &payloadAge{payload: payload, age: math.MaxInt64}
	buffer.newlyBuffered <- payloadAge
	pfxlog.ContextLogger("s/"+payload.GetSessionId()).Debugf("buffered [%d]", payload.GetSequence())
	return payloadAge.markSent, nil
}

func (buffer *PayloadBuffer) PayloadReceived(payload *Payload) {
	defer func() {
		if r := recover(); r != nil {
			pfxlog.ContextLogger("s/" + buffer.x.sessionId).Error("send on closed channel")
		}
	}()
	pfxlog.Logger().WithFields(payload.GetLoggerFields()).Debug("acknowledging")
	buffer.transmitBuffer.ReceiveUnordered(payload)
	buffer.newlyAcknowledged <- payload
	pfxlog.ContextLogger("s/"+payload.GetSessionId()).Debugf("acknowledge [%d]", payload.GetSequence())
}

func (buffer *PayloadBuffer) ReceiveAcknowledgement(ack *Acknowledgement) {
	defer func() {
		if r := recover(); r != nil {
			pfxlog.ContextLogger("s/" + buffer.x.sessionId).Error("send on closed channel")
		}
	}()
	buffer.newlyReceivedAcks <- ack
	pfxlog.ContextLogger("s/"+ack.SessionId).Debugf("received ack [%d]", len(ack.Sequence))
}

func (buffer *PayloadBuffer) Close() {
	logrus.Debugf("[%p] closing", buffer)
	defer func() {
		if r := recover(); r != nil {
			pfxlog.Logger().Debug("already closed")
		}
	}()
	close(buffer.newlyBuffered)
	close(buffer.newlyAcknowledged)
	close(buffer.newlyReceivedAcks)
}

func (buffer *PayloadBuffer) run() {
	log := pfxlog.ContextLogger("s/" + buffer.x.sessionId)
	defer log.Debugf("[%p] exited", buffer)
	log.Debugf("[%p] started", buffer)

	lastDebug := info.NowInMilliseconds()

	var buffered chan *payloadAge

	for {
		if buffer.freeSpace > 0 {
			buffered = buffer.newlyBuffered
		} else {
			buffered = nil
		}

		now := info.NowInMilliseconds()
		if now-lastDebug >= 2000 {
			buffer.debug(now)
			lastDebug = now
		}

		select {
		case ack := <-buffer.newlyReceivedAcks:
			if ack != nil {
				if err := buffer.receiveAcknowledgement(ack); err != nil {
					log.Errorf("unexpected error (%s)", err)
				}
			} else {
				return
			}
			if err := buffer.retransmit(); err != nil {
				log.Errorf("unexpected error retransmitting (%s)", err)
			}

		case payload := <-buffer.newlyAcknowledged:
			if payload != nil {
				if err := buffer.acknowledgePayload(payload); err != nil {
					log.Errorf("unexpected error (%s)", err)
				} else if payload.RTT != 0 {
					buffer.mostRecentRTT = uint16(info.NowInMilliseconds()) - payload.RTT
				}
				if err := buffer.acknowledge(); err != nil {
					log.Errorf("unexpected error (%s)", err)
				}
			} else {
				return
			}

		case payloadAge := <-buffered:
			if payloadAge != nil {
				buffer.buffer[payloadAge.payload.GetSequence()] = payloadAge
				buffer.freeSpace -= uint32(len(payloadAge.payload.Data))
				if err := buffer.acknowledge(); err != nil {
					log.Errorf("unexpected error (%s)", err)
				}
			} else {
				return
			}

		case <-time.After(time.Duration(buffer.config.idleAckAfter) * time.Millisecond):
			if err := buffer.acknowledge(); err != nil {
				log.Errorf("unexpected error acknowledging (%s)", err)
			}
			if err := buffer.retransmit(); err != nil {
				log.Errorf("unexpected error retransmitting (%s)", err)
			}
		}
	}
}

func (buffer *PayloadBuffer) acknowledgePayload(payload *Payload) error {
	if buffer.x.sessionId == payload.SessionId {
		buffer.acked[payload.Sequence] = info.NowInMilliseconds()
	} else {
		return errors.New("unexpected Payload")
	}

	return nil
}

func (buffer *PayloadBuffer) receiveAcknowledgement(ack *Acknowledgement) error {
	log := pfxlog.ContextLogger("s/" + buffer.x.sessionId)
	if buffer.x.sessionId == ack.SessionId {
		for _, sequence := range ack.Sequence {
			if sequence > buffer.receivedAckHwm {
				buffer.receivedAckHwm = sequence
			}
			delete(buffer.buffer, sequence)
			log.Debugf("acknowledged sequence [%d]", sequence)
		}
		buffer.freeSpace = ack.FreeSpace
		if ack.RTT > 0 {
			buffer.config.retransmitAge = ack.RTT
			rttHistogram.Update(int64(ack.RTT))
		}
	} else {
		return errors.New("unexpected acknowledgement")
	}
	return nil
}

func (buffer *PayloadBuffer) acknowledge() error {
	log := pfxlog.ContextLogger("s/" + buffer.x.sessionId)
	now := info.NowInMilliseconds()

	if now-buffer.lastAck >= buffer.config.ackPeriod || len(buffer.acked) >= buffer.config.ackCount {
		log.Debug("ready to acknowledge")

		ack := NewAcknowledgement(buffer.x.sessionId, buffer.x.originator)
		freeSpace := 4*64*1024 - buffer.transmitBuffer.Size()
		if freeSpace < 0 {
			freeSpace = 0
		}
		ack.FreeSpace = uint32(freeSpace)
		for sequence := range buffer.acked {
			ack.Sequence = append(ack.Sequence, sequence)
		}
		if buffer.mostRecentRTT != 0 {
			ack.RTT = buffer.mostRecentRTT
			buffer.mostRecentRTT = 0
		}
		log.Debugf("acknowledging [%d] payloads, [%d] buffered", len(ack.Sequence), len(buffer.buffer))

		buffer.controller.ack(ack, buffer.x.address)

		buffer.acked = make(map[int32]int64) // clear
		buffer.lastAck = now

	} else {
		log.Debug("not ready to acknowledge")
	}

	return nil
}

func (buffer *PayloadBuffer) retransmit() error {
	if len(buffer.buffer) > 0 {
		log := pfxlog.ContextLogger(fmt.Sprintf("s/" + buffer.x.sessionId))

		now := info.NowInMilliseconds()
		retransmitted := 0
		for _, v := range buffer.buffer {
			if v.payload.GetSequence() < buffer.receivedAckHwm && uint16(now-v.getAge()) > buffer.config.retransmitAge {
				buffer.controller.retransmit(v, buffer.x.address)
				retransmitted++
			}
		}

		if retransmitted > 0 {
			log.Infof("retransmitted [%d] payloads, [%d] buffered", retransmitted, len(buffer.buffer))
		}
	}
	return nil
}

func (buffer *PayloadBuffer) debug(now int64) {
	pfxlog.ContextLogger(buffer.x.sessionId).Debugf("buffer=[%d], acked=[%d], lastAck=[%d ms.], receivedAckHwm=[%d]",
		len(buffer.buffer), len(buffer.acked), now-buffer.lastAck, buffer.receivedAckHwm)
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
