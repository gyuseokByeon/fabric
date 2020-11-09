package xgress

import (
	"github.com/biogo/store/llrb"
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/foundation/metrics"
	"reflect"
	"strings"
	"sync/atomic"
)

var retransmitter *Retransmitter

func InitRetransmitter(forwarder PayloadBufferForwarder, metrics metrics.Registry) {
	retransmitter = NewRetransmitter(forwarder, metrics)
}

type Retransmitter struct {
	forwarder            PayloadBufferForwarder
	retransmits          *llrb.Tree
	retransmitIngest     chan *retransmit
	retransmitSend       chan *retransmit
	retransmitsQueueSize int64
}

func NewRetransmitter(forwarder PayloadBufferForwarder, metrics metrics.Registry) *Retransmitter {
	ctrl := &Retransmitter{
		forwarder:        forwarder,
		retransmits:      &llrb.Tree{},
		retransmitIngest: make(chan *retransmit, 16),
		retransmitSend:   make(chan *retransmit, 1),
	}

	go ctrl.retransmitIngester()
	go ctrl.retransmitSender()

	metrics.FuncGauge("xgress.retransmits.queue_size", func() int64 {
		return atomic.LoadInt64(&ctrl.retransmitsQueueSize)
	})

	return ctrl
}

func (retransmitter *Retransmitter) queue(p *payloadAge, x *Xgress) {
	retransmitter.retransmitIngest <- &retransmit{
		payloadAge: p,
		x:          x,
	}
}

func (retransmitter *Retransmitter) retransmitIngester() {
	var next *retransmit
	for {
		if next == nil && retransmitter.retransmits.Count > 0 {
			next = retransmitter.retransmits.Max().(*retransmit)
			retransmitter.retransmits.DeleteMax()
		}

		if next == nil {
			select {
			case retransmit := <-retransmitter.retransmitIngest:
				retransmitter.acceptRetransmit(retransmit)
			}
		} else {
			select {
			case retransmit := <-retransmitter.retransmitIngest:
				retransmitter.acceptRetransmit(retransmit)
			case retransmitter.retransmitSend <- next:
				next = nil
			}
		}
		atomic.StoreInt64(&retransmitter.retransmitsQueueSize, int64(retransmitter.retransmits.Count))
	}
}

func (retransmitter *Retransmitter) acceptRetransmit(r *retransmit) {
	if r.isAcked() {
		if retransmitter.retransmits.Get(r) != nil {
			retransmitter.retransmits.Delete(r)
		}
	} else {
		retransmitter.retransmits.Insert(r)
	}
}

func (retransmitter *Retransmitter) retransmitSender() {
	logger := pfxlog.Logger()
	for retransmit := range retransmitter.retransmitSend {
		if !retransmit.isAcked() {
			if err := retransmitter.forwarder.ForwardPayload(retransmit.x.address, retransmit.payloadAge.payload); err != nil {
				logger.WithError(err).Errorf("unexpected error while retransmitting payload from %v", retransmit.x.address)
				retransmissionFailures.Mark(1)
			} else {
				retransmit.markSent()
				retransmissions.Mark(1)
			}
			retransmit.dequeued()
		}
	}
}

type retransmit struct {
	*payloadAge
	x *Xgress
}

func (r *retransmit) Compare(comparable llrb.Comparable) int {
	if other, ok := comparable.(*retransmit); ok {
		if result := int(r.age - other.age); result != 0 {
			return result
		}
		if result := int(r.payload.Sequence - other.payload.Sequence); result != 0 {
			return result
		}
		return strings.Compare(r.x.sessionId, other.x.sessionId)
	}
	pfxlog.Logger().Errorf("*retransmit was compared to %v, which should not happen", reflect.TypeOf(comparable))
	return -1
}
