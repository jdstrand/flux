package execute

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/apache/arrow/go/arrow/memory"
	"github.com/influxdata/flux"
	"github.com/influxdata/flux/codes"
	"github.com/influxdata/flux/internal/errors"
	"github.com/influxdata/flux/internal/execute/table"
	"github.com/influxdata/flux/interpreter"
	"github.com/influxdata/flux/plan"
)

// Transport is an interface for handling raw messages.
type Transport interface {
	// ProcessMessage will process a message in the Transport.
	ProcessMessage(m Message) error
}

// AsyncTransport is a Transport that performs its work in a separate goroutine.
type AsyncTransport interface {
	Transport
	// Finished reports when the AsyncTransport has completed and there is no more work to do.
	Finished() <-chan struct{}
}

var _ Transformation = (*consecutiveTransport)(nil)

// consecutiveTransport implements AsyncTransport by transporting data consecutively to the downstream Transformation.
type consecutiveTransport struct {
	dispatcher Dispatcher

	t        Transport
	messages MessageQueue
	label    string
	stack    []interpreter.StackEntry

	finished chan struct{}
	errMu    sync.Mutex
	errValue error

	schedulerState int32
	inflight       int32
}

func newConsecutiveTransport(dispatcher Dispatcher, t Transformation, n plan.Node, mem memory.Allocator) *consecutiveTransport {
	return &consecutiveTransport{
		dispatcher: dispatcher,
		t:          WrapTransformationInTransport(t, mem),
		// TODO(nathanielc): Have planner specify message queue initial buffer size.
		messages: newMessageQueue(64),
		label:    string(n.ID()),
		stack:    n.CallStack(),
		finished: make(chan struct{}),
	}
}

func (t *consecutiveTransport) sourceInfo() string {
	if len(t.stack) == 0 {
		return ""
	}

	// Learn the filename from the bottom of the stack.
	// We want the top most entry (deepest in the stack)
	// from the primary file. We can retrieve the filename
	// for the primary file by looking at the bottom of the
	// stack and then finding the top-most entry with that
	// filename.
	filename := t.stack[len(t.stack)-1].Location.File
	for i := 0; i < len(t.stack); i++ {
		entry := t.stack[i]
		if entry.Location.File == filename {
			return fmt.Sprintf("@%s: %s", entry.Location, entry.FunctionName)
		}
	}
	entry := t.stack[0]
	return fmt.Sprintf("@%s: %s", entry.Location, entry.FunctionName)
}
func (t *consecutiveTransport) setErr(err error) {
	t.errMu.Lock()
	msg := "runtime error"
	if srcInfo := t.sourceInfo(); srcInfo != "" {
		msg += " " + srcInfo
	}
	err = errors.Wrap(err, codes.Inherit, msg)
	t.errValue = err
	t.errMu.Unlock()
}
func (t *consecutiveTransport) err() error {
	t.errMu.Lock()
	err := t.errValue
	t.errMu.Unlock()
	return err
}

func (t *consecutiveTransport) Finished() <-chan struct{} {
	return t.finished
}

func (t *consecutiveTransport) RetractTable(id DatasetID, key flux.GroupKey) error {
	select {
	case <-t.finished:
		return t.err()
	default:
	}
	t.pushMsg(&retractTableMsg{
		srcMessage: srcMessage(id),
		key:        key,
	})
	return nil
}

func (t *consecutiveTransport) Process(id DatasetID, tbl flux.Table) error {
	select {
	case <-t.finished:
		return t.err()
	default:
	}
	t.pushMsg(&processMsg{
		srcMessage: srcMessage(id),
		table:      tbl,
	})
	return nil
}

func (t *consecutiveTransport) UpdateWatermark(id DatasetID, time Time) error {
	select {
	case <-t.finished:
		return t.err()
	default:
	}
	t.pushMsg(&updateWatermarkMsg{
		srcMessage: srcMessage(id),
		time:       time,
	})
	return nil
}

func (t *consecutiveTransport) UpdateProcessingTime(id DatasetID, time Time) error {
	select {
	case <-t.finished:
		return t.err()
	default:
	}
	t.pushMsg(&updateProcessingTimeMsg{
		srcMessage: srcMessage(id),
		time:       time,
	})
	return nil
}

func (t *consecutiveTransport) Finish(id DatasetID, err error) {
	select {
	case <-t.finished:
		return
	default:
	}
	t.pushMsg(&finishMsg{
		srcMessage: srcMessage(id),
		err:        err,
	})
}

func (t *consecutiveTransport) pushMsg(m Message) {
	t.messages.Push(m)
	atomic.AddInt32(&t.inflight, 1)
	t.schedule()
}

func (t *consecutiveTransport) ProcessMessage(m Message) error {
	t.pushMsg(m)
	return nil
}

const (
	// consecutiveTransport schedule states
	idle int32 = iota
	running
	finished
)

// schedule indicates that there is work available to schedule.
func (t *consecutiveTransport) schedule() {
	if t.tryTransition(idle, running) {
		t.dispatcher.Schedule(t.processMessages)
	}
}

// tryTransition attempts to transition into the new state and returns true on success.
func (t *consecutiveTransport) tryTransition(old, new int32) bool {
	return atomic.CompareAndSwapInt32(&t.schedulerState, old, new)
}

// transition sets the new state.
func (t *consecutiveTransport) transition(new int32) {
	atomic.StoreInt32(&t.schedulerState, new)
}

func (t *consecutiveTransport) processMessages(ctx context.Context, throughput int) {
PROCESS:
	i := 0
	for m := t.messages.Pop(); m != nil; m = t.messages.Pop() {
		atomic.AddInt32(&t.inflight, -1)
		if f, err := t.processMessage(ctx, m); err != nil || f {
			// Set the error if there was any
			t.setErr(err)

			// Transition to the finished state.
			if t.tryTransition(running, finished) {
				// Call Finish if we have not already
				if !f {
					m := &finishMsg{
						srcMessage: srcMessage(m.SrcDatasetID()),
						err:        t.err(),
					}
					_ = t.t.ProcessMessage(m)
				}
				// We are finished
				close(t.finished)
				return
			}
		}
		i++
		if i >= throughput {
			// We have done enough work.
			// Transition to the idle state and reschedule for later.
			t.transition(idle)
			t.schedule()
			return
		}
	}

	t.transition(idle)
	// Check if more messages arrived after the above loop finished.
	// This check must happen in the idle state.
	if atomic.LoadInt32(&t.inflight) > 0 {
		if t.tryTransition(idle, running) {
			goto PROCESS
		} // else we have already been scheduled again, we can return
	}
}

// processMessage processes the message on t.
// The return value is true if the message was a FinishMsg.
func (t *consecutiveTransport) processMessage(ctx context.Context, m Message) (finished bool, err error) {
	if _, span := StartSpanFromContext(ctx, reflect.TypeOf(t.t).String(), t.label); span != nil {
		defer span.Finish()
	}
	if err := t.t.ProcessMessage(m); err != nil {
		return false, err
	}
	finished = isFinishMessage(m)
	return finished, nil
}

type Message interface {
	Type() MessageType
	SrcDatasetID() DatasetID
}

type MessageType int

const (
	// RetractTableType is sent when the previous table for
	// a given group key should be retracted.
	RetractTableType MessageType = iota

	// ProcessType is sent when there is an entire flux.Table
	// ready to be processed from the upstream Dataset.
	ProcessType

	// UpdateWatermarkType is sent when there will be no more
	// points older than the watermark for any key.
	UpdateWatermarkType

	// UpdateProcessingTimeType is sent to update the current time.
	UpdateProcessingTimeType

	// FinishType is sent when there are no more messages from
	// the upstream Dataset or an upstream error occurred that
	// caused the execution to abort.
	FinishType
)

type srcMessage DatasetID

func (m srcMessage) SrcDatasetID() DatasetID {
	return DatasetID(m)
}

type RetractTableMsg interface {
	Message
	Key() flux.GroupKey
}

type retractTableMsg struct {
	srcMessage
	key flux.GroupKey
}

func (m *retractTableMsg) Type() MessageType {
	return RetractTableType
}
func (m *retractTableMsg) Key() flux.GroupKey {
	return m.key
}

type ProcessMsg interface {
	Message
	Table() flux.Table
}

type processMsg struct {
	srcMessage
	table flux.Table
}

func (m *processMsg) Type() MessageType {
	return ProcessType
}
func (m *processMsg) Table() flux.Table {
	return m.table
}

type UpdateWatermarkMsg interface {
	Message
	WatermarkTime() Time
}

type updateWatermarkMsg struct {
	srcMessage
	time Time
}

func (m *updateWatermarkMsg) Type() MessageType {
	return UpdateWatermarkType
}
func (m *updateWatermarkMsg) WatermarkTime() Time {
	return m.time
}

type UpdateProcessingTimeMsg interface {
	Message
	ProcessingTime() Time
}

type updateProcessingTimeMsg struct {
	srcMessage
	time Time
}

func (m *updateProcessingTimeMsg) Type() MessageType {
	return UpdateProcessingTimeType
}
func (m *updateProcessingTimeMsg) ProcessingTime() Time {
	return m.time
}

type FinishMsg interface {
	Message
	Error() error
}

type finishMsg struct {
	srcMessage
	err error
}

func (m *finishMsg) Type() MessageType {
	return FinishType
}
func (m *finishMsg) Error() error {
	return m.err
}

// transformationTransportAdapter will translate Message values sent to
// a Transport to an underlying Transformation.
type transformationTransportAdapter struct {
	t     Transformation
	cache table.BuilderCache
}

// WrapTransformationInTransport will wrap a Transformation into
// a Transport to be used for the execution engine.
func WrapTransformationInTransport(t Transformation, mem memory.Allocator) Transport {
	// If the Transformation implements the Transport interface,
	// then we can just use that directly.
	if tr, ok := t.(Transport); ok {
		return tr
	}
	return &transformationTransportAdapter{
		t: t,
		cache: table.BuilderCache{
			New: func(key flux.GroupKey) table.Builder {
				return table.NewBufferedBuilder(key, mem)
			},
		},
	}
}

func (t *transformationTransportAdapter) ProcessMessage(m Message) error {
	switch m := m.(type) {
	case RetractTableMsg:
		return t.t.RetractTable(m.SrcDatasetID(), m.Key())
	case ProcessMsg:
		b := m.Table()
		return t.t.Process(m.SrcDatasetID(), b)
	case UpdateWatermarkMsg:
		return t.t.UpdateWatermark(m.SrcDatasetID(), m.WatermarkTime())
	case UpdateProcessingTimeMsg:
		return t.t.UpdateProcessingTime(m.SrcDatasetID(), m.ProcessingTime())
	case FinishMsg:
		t.t.Finish(m.SrcDatasetID(), m.Error())
		return nil
	default:
		// Message is not handled by older Transformation implementations.
		return nil
	}
}

// isFinishMessage will return true if the Message is a FinishMsg.
func isFinishMessage(m Message) bool {
	_, ok := m.(FinishMsg)
	return ok
}
