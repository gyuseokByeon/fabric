package xgress

import (
	"github.com/biogo/store/llrb"
	"github.com/michaelquigley/pfxlog"
	"github.com/openziti/foundation/metrics"
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

func (retransmitter *Retransmitter) retransmit(p *payloadAge, address Address) {
	retransmitter.retransmitIngest <- &retransmit{
		payloadAge: p,
		Address:    address,
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
				retransmitter.retransmits.Insert(retransmit)
			}
		} else {
			select {
			case retransmit := <-retransmitter.retransmitIngest:
				retransmitter.retransmits.Insert(retransmit)
			case retransmitter.retransmitSend <- next:
				next = nil
			}
		}
		atomic.StoreInt64(&retransmitter.retransmitsQueueSize, int64(retransmitter.retransmits.Count))
	}
}

func (retransmitter *Retransmitter) retransmitSender() {
	logger := pfxlog.Logger()
	for retransmit := range retransmitter.retransmitSend {
		if err := retransmitter.forwarder.ForwardPayload(retransmit.Address, retransmit.payloadAge.payload); err != nil {
			logger.WithError(err).Errorf("unexpected error while retransmitting payload from %v", retransmit.Address)
			retransmissionFailures.Mark(1)
		} else {
			retransmissions.Mark(1)
			retransmit.markSent()
		}
	}
}
