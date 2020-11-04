package xgress

import (
	"github.com/openziti/foundation/metrics"
	"sync/atomic"
)

var ackTxMeter metrics.Meter
var ackRxMeter metrics.Meter
var droppedPayloadsMeter metrics.Meter
var retransmissions metrics.Meter
var retransmissionFailures metrics.Meter

var ackFailures metrics.Meter
var rttHistogram metrics.Histogram
var rxBufferSizeHistogram metrics.Histogram
var localTxBufferSizeHistogram metrics.Histogram
var remoteTxBufferSizeHistogram metrics.Histogram
var payloadWriteTimer metrics.Timer
var ackWriteTimer metrics.Timer
var payloadBufferTimer metrics.Timer
var payloadRelayTimer metrics.Timer

var buffersBlockedByLocalWindow int64
var buffersBlockedByRemoteWindow int64

func InitMetrics(registry metrics.UsageRegistry) {
	droppedPayloadsMeter = registry.Meter("xgress.dropped_payloads")
	retransmissions = registry.Meter("xgress.retransmissions")
	retransmissionFailures = registry.Meter("xgress.retransmission_failures")
	ackRxMeter = registry.Meter("xgress.rx.acks")
	ackTxMeter = registry.Meter("xgress.tx.acks")
	ackFailures = registry.Meter("xgress.ack_failures")
	rttHistogram = registry.Histogram("xgress.rtt")
	rxBufferSizeHistogram = registry.Histogram("xgress.rx_buffer_size")
	localTxBufferSizeHistogram = registry.Histogram("xgress.local.tx_buffer_size")
	remoteTxBufferSizeHistogram = registry.Histogram("xgress.remote.tx_buffer_size")
	payloadWriteTimer = registry.Timer("xgress.tx_write_time")
	ackWriteTimer = registry.Timer("xgress.ack_write_time")
	payloadBufferTimer = registry.Timer("xgress.payload_buffer_time")
	payloadRelayTimer = registry.Timer("xgress.payload_relay_time")

	registry.FuncGauge("xgress.blocked_by_local_window", func() int64 {
		return atomic.LoadInt64(&buffersBlockedByLocalWindow)
	})

	registry.FuncGauge("xgress.blocked_by_remote_window", func() int64 {
		return atomic.LoadInt64(&buffersBlockedByRemoteWindow)
	})
}
