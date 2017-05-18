// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cadence

// All code in this file is private to the package.

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.uber.org/zap"
)

type (
	syncWorkflowDefinition struct {
		workflow        workflow
		dispatcher      dispatcher
		cancel          CancelFunc
		cancelRequested bool
		rootCtx         Context
	}

	workflowResult struct {
		workflowResult []byte
		error          error
	}

	activityClient struct {
		dispatcher  dispatcher
		asyncClient asyncActivityClient
	}

	futureImpl struct {
		value   interface{}
		err     error
		ready   bool
		channel *channelImpl
		chained []asyncFuture // Futures that are chained to this one
	}

	// Dispatcher is a container of a set of coroutines.
	dispatcher interface {
		// ExecuteUntilAllBlocked executes coroutines one by one in deterministic order
		// until all of them are completed or blocked on Channel or Selector
		ExecuteUntilAllBlocked() (err PanicError)
		// IsDone returns true when all of coroutines are completed
		IsDone() bool
		Close()             // Destroys all coroutines without waiting for their completion
		StackTrace() string // Stack trace of all coroutines owned by the Dispatcher instance
	}

	// Workflow is an interface that any workflow should implement.
	// Code of a workflow must be deterministic. It must use cadence.Channel, cadence.Selector, and cadence.Go instead of
	// native channels, select and go. It also must not use range operation over map as it is randomized by go runtime.
	// All time manipulation should use current time returned by GetTime(ctx) method.
	// Note that cadence.Context is used instead of context.Context to avoid use of raw channels.
	workflow interface {
		Execute(ctx Context, input []byte) (result []byte, err error)
	}

	valueCallbackPair struct {
		value    interface{}
		callback func() bool // false indicates that callback didn't accept the value
	}

	// false result means that callback didn't accept the value and it is still up for delivery
	receiveCallback func(v interface{}, more bool) bool

	channelImpl struct {
		name            string              // human readable channel name
		size            int                 // Channel buffer size. 0 for non buffered.
		buffer          []interface{}       // buffered messages
		blockedSends    []valueCallbackPair // puts waiting when buffer is full.
		blockedReceives []receiveCallback   // receives waiting when no messages are available.
		closed          bool                // true if channel is closed
	}

	// Single case statement of the Select
	selectCase struct {
		channel                 *channelImpl                    // Channel of this case.
		receiveFunc             *func(v interface{})            // function to call when channel has a message. nil for send case.
		receiveWithMoreFlagFunc *func(v interface{}, more bool) // function to call when channel has a message. nil for send case.

		sendFunc   *func()         // function to call when channel accepted a message. nil for receive case.
		sendValue  *interface{}    // value to send to the channel. Used only for send case.
		future     asyncFuture     // Used for future case
		futureFunc *func(f Future) // function to call when Future is ready
	}

	// Implements Selector interface
	selectorImpl struct {
		name        string
		cases       []*selectCase // cases that this select is comprised from
		defaultFunc *func()       // default case
	}

	// unblockFunc is passed evaluated by a coroutine yield. When it returns false the yield returns to a caller.
	// stackDepth is the depth of stack from the last blocking call relevant to user.
	// Used to truncate internal stack frames from thread stack.
	unblockFunc func(status string, stackDepth int) (keepBlocked bool)

	coroutineState struct {
		name         string
		dispatcher   *dispatcherImpl  // dispatcher this context belongs to
		aboutToBlock chan bool        // used to notify dispatcher that coroutine that owns this context is about to block
		unblock      chan unblockFunc // used to notify coroutine that it should continue executing.
		keptBlocked  bool             // true indicates that coroutine didn't make any progress since the last yield unblocking
		closed       bool             // indicates that owning coroutine has finished execution
		panicError   PanicError       // non nil if coroutine had unhandled panic
	}

	dispatcherImpl struct {
		sequence         int
		channelSequence  int // used to name channels
		selectorSequence int // used to name channels
		coroutines       []*coroutineState
		executing        bool       // currently running ExecuteUntilAllBlocked. Used to avoid recursive calls to it.
		mutex            sync.Mutex // used to synchronize executing
		closed           bool
	}

	asyncFuture interface {
		Future
		// Used by selectorImpl
		// If Future is ready returns its value immediately.
		// If not registers callback which is called when it is ready.
		GetAsync(callback receiveCallback) (v interface{}, ok bool, err error)

		// This future will added to list of dependency futures.
		ChainFuture(f Future)

		// Gets the current value and error.
		// Make sure this is called once the future is ready.
		GetValueAndError() (v interface{}, err error)

		Set(value interface{}, err error)
	}
)

const workflowEnvironmentContextKey = "workflowEnv"
const workflowResultContextKey = "workflowResult"
const coroutinesContextKey = "coroutines"

// Assert that structs do indeed implement the interfaces
var _ Channel = (*channelImpl)(nil)
var _ Selector = (*selectorImpl)(nil)
var _ dispatcher = (*dispatcherImpl)(nil)

var stackBuf [100000]byte

// Pointer to pointer to workflow result
func getWorkflowResultPointerPointer(ctx Context) **workflowResult {
	rpp := ctx.Value(workflowResultContextKey)
	if rpp == nil {
		panic("getWorkflowResultPointerPointer: Not a workflow context")
	}
	return rpp.(**workflowResult)
}

func getWorkflowEnvironment(ctx Context) workflowEnvironment {
	wc := ctx.Value(workflowEnvironmentContextKey)
	if wc == nil {
		panic("getWorkflowContext: Not a workflow context")
	}
	return wc.(workflowEnvironment)
}

func (f *futureImpl) Get(ctx Context, value interface{}) error {
	_, more := f.channel.ReceiveWithMoreFlag(ctx)
	if more {
		panic("not closed")
	}
	if !f.ready {
		panic("not ready")
	}
	if value == nil {
		return f.err
	}
	rf := reflect.ValueOf(value)
	if rf.Type().Kind() != reflect.Ptr {
		return errors.New("value parameter is not a pointer")
	}
	fv := reflect.ValueOf(f.value)
	if fv.IsValid() {
		rf.Elem().Set(fv)
	}
	return f.err
}

// Used by selectorImpl
// If Future is ready returns its value immediately.
// If not registers callback which is called when it is ready.
func (f *futureImpl) GetAsync(callback receiveCallback) (v interface{}, ok bool, err error) {
	_, _, more := f.channel.receiveAsyncImpl(callback)
	// Future uses Channel.Close to indicate that it is ready.
	// So more being true (channel is still open) indicates future is not ready.
	if more {
		return nil, false, nil
	}
	if !f.ready {
		panic("not ready")
	}
	return f.value, true, f.err
}

func (f *futureImpl) IsReady() bool {
	return f.ready
}

func (f *futureImpl) Set(value interface{}, err error) {
	if f.ready {
		panic("already set")
	}
	f.value = value
	f.err = err
	f.ready = true
	f.channel.Close()
	for _, ch := range f.chained {
		ch.Set(f.value, f.err)
	}
}

func (f *futureImpl) SetValue(value interface{}) {
	if f.ready {
		panic("already set")
	}
	f.Set(value, nil)
}

func (f *futureImpl) SetError(err error) {
	if f.ready {
		panic("already set")
	}
	f.Set(nil, err)
}

func (f *futureImpl) Chain(future Future) {
	if f.ready {
		panic("already set")
	}

	ch, ok := future.(asyncFuture)
	if !ok {
		panic("cannot chain Future that wasn't created with cadence.NewFuture")
	}
	if !ch.IsReady() {
		ch.ChainFuture(f)
		return
	}
	val, err := ch.GetValueAndError()
	f.value = val
	f.err = err
	f.ready = true
	return
}

func (f *futureImpl) ChainFuture(future Future) {
	f.chained = append(f.chained, future.(asyncFuture))
}

func (f *futureImpl) GetValueAndError() (interface{}, error) {
	return f.value, f.err
}

func (d *syncWorkflowDefinition) Execute(env workflowEnvironment, input []byte) {
	d.rootCtx = WithValue(background, workflowEnvironmentContextKey, env)
	var resultPtr *workflowResult
	d.rootCtx = WithValue(d.rootCtx, workflowResultContextKey, &resultPtr)

	// Set default values for the workflow execution.
	wInfo := env.WorkflowInfo()
	d.rootCtx = WithWorkflowDomain(d.rootCtx, wInfo.Domain)
	d.rootCtx = WithWorkflowTaskList(d.rootCtx, wInfo.TaskListName)
	d.rootCtx = WithExecutionStartToCloseTimeout(d.rootCtx, time.Duration(wInfo.ExecutionStartToCloseTimeoutSeconds)*time.Second)
	d.rootCtx = WithWorkflowTaskStartToCloseTimeout(d.rootCtx, time.Duration(wInfo.TaskStartToCloseTimeoutSeconds)*time.Second)
	d.rootCtx = WithTaskList(d.rootCtx, wInfo.TaskListName)
	activityOptions := getActivityOptions(d.rootCtx)
	activityOptions.OriginalTaskListName = wInfo.TaskListName

	// There is a inter dependency, before we call Execute() we can have a cancel request since
	// dispatcher executes code on decision task started, we might not have cancel handler created.
	// WithCancel -> creates channel -> needs dispatcher -> dispatcher needs a root function with context.
	// We use cancelRequested to remember if the cancel request came in.

	d.dispatcher = newDispatcher(d.rootCtx, func(ctx Context) {
		ctx, d.cancel = WithCancel(ctx)
		if d.cancelRequested {
			d.cancel()
		}
		r := &workflowResult{}
		r.workflowResult, r.error = d.workflow.Execute(ctx, input)
		rpp := getWorkflowResultPointerPointer(ctx)
		*rpp = r
	})

	getWorkflowEnvironment(d.rootCtx).RegisterCancel(func() {
		// It is ok to call this method multiple times.
		// it doesn't do anything new, the context remains cancelled.
		if d.cancel != nil {
			d.cancel()
		}
		d.cancelRequested = true
	})
}

func (d *syncWorkflowDefinition) OnDecisionTaskStarted() {
	executeDispatcher(d.rootCtx, d.dispatcher)
}

func (d *syncWorkflowDefinition) StackTrace() string {
	return d.dispatcher.StackTrace()
}

func (d *syncWorkflowDefinition) Close() {
	if d.dispatcher != nil {
		d.dispatcher.Close()
	}
}

// NewDispatcher creates a new Dispatcher instance with a root coroutine function.
// Context passed to the root function is child of the passed rootCtx.
// This way rootCtx can be used to pass values to the coroutine code.
func newDispatcher(rootCtx Context, root func(ctx Context)) dispatcher {
	result := &dispatcherImpl{}
	result.newCoroutine(rootCtx, root)
	return result
}

// getDispatcher retrieves current dispatcher from the Context passed to the coroutine function.
func getDispatcher(ctx Context) dispatcher {
	return getState(ctx).dispatcher
}

// executeDispatcher executed coroutines in the calling thread and calls workflow completion callbacks
// if root workflow function returned
func executeDispatcher(ctx Context, dispatcher dispatcher) {
	panicErr := dispatcher.ExecuteUntilAllBlocked()
	if panicErr != nil {
		env := getWorkflowEnvironment(ctx)
		env.GetLogger().Error("Dispatcher panic.",
			zap.String("PanicError", panicErr.Error()),
			zap.String("PanicStack", panicErr.StackTrace()))
		env.Complete(nil, NewErrorWithDetails(panicErr.Error(), []byte(panicErr.StackTrace())))
		return
	}
	rp := *getWorkflowResultPointerPointer(ctx)
	if rp == nil {
		// Result is not set, so workflow is still executing
		return
	}
	// Cannot cast nil values from interface{} to interface
	getWorkflowEnvironment(ctx).Complete(rp.workflowResult, rp.error)
}

// For troubleshooting stack pretty printing only.
// Set to true to see full stack trace that includes framework methods.
const disableCleanStackTraces = false

func getState(ctx Context) *coroutineState {
	s := ctx.Value(coroutinesContextKey)
	if s == nil {
		panic("getState: not workflow context")
	}
	return s.(*coroutineState)
}

func (c *channelImpl) Receive(ctx Context) (v interface{}) {
	v, _ = c.ReceiveWithMoreFlag(ctx)
	return v
}

func (c *channelImpl) ReceiveWithMoreFlag(ctx Context) (value interface{}, more bool) {
	state := getState(ctx)
	hasResult := false
	var result interface{}
	callback := func(v interface{}, m bool) bool {
		result = v
		hasResult = true
		more = m
		return true
	}
	v, ok, more := c.receiveAsyncImpl(callback)
	if ok || !more {
		return v, more
	}
	for {
		if hasResult {
			state.unblocked()
			return result, more
		}
		state.yield(fmt.Sprintf("blocked on %s.Receive", c.name))
	}
}

func (c *channelImpl) ReceiveAsync() (v interface{}, ok bool) {
	v, ok, _ = c.ReceiveAsyncWithMoreFlag()
	return v, ok
}

func (c *channelImpl) ReceiveAsyncWithMoreFlag() (v interface{}, ok bool, more bool) {
	return c.receiveAsyncImpl(nil)
}

// ok = true means that value was received
// more = true means that channel is not closed and more deliveries are possible
func (c *channelImpl) receiveAsyncImpl(callback receiveCallback) (v interface{}, ok bool, more bool) {
	if len(c.buffer) > 0 {
		r := c.buffer[0]
		c.buffer = c.buffer[1:]
		return r, true, true
	}
	if c.closed {
		return nil, false, false
	}
	for len(c.blockedSends) > 0 {
		b := c.blockedSends[0]
		c.blockedSends = c.blockedSends[1:]
		if b.callback() {
			return b.value, true, true
		}
	}
	if callback != nil {
		c.blockedReceives = append(c.blockedReceives, callback)
	}
	return nil, false, true
}

func (c *channelImpl) Send(ctx Context, v interface{}) {
	state := getState(ctx)
	valueConsumed := false
	pair := &valueCallbackPair{
		value: v,
		callback: func() bool {
			valueConsumed = true
			return true
		},
	}
	ok := c.sendAsyncImpl(v, pair)
	if ok {
		state.unblocked()
		return
	}
	for {
		// Check for closed in the loop as close can be called when send is blocked
		if c.closed {
			panic("Closed channel")
		}
		if valueConsumed {
			state.unblocked()
			return
		}
		state.yield(fmt.Sprintf("blocked on %s.Send", c.name))
	}
}

func (c *channelImpl) SendAsync(v interface{}) (ok bool) {
	return c.sendAsyncImpl(v, nil)
}

func (c *channelImpl) sendAsyncImpl(v interface{}, pair *valueCallbackPair) (ok bool) {
	if c.closed {
		panic("Closed channel")
	}
	for len(c.blockedReceives) > 0 {
		blockedGet := c.blockedReceives[0]
		c.blockedReceives = c.blockedReceives[1:]
		// false from callback indicates that value wasn't consumed
		if blockedGet(v, true) {
			return true
		}
	}
	if len(c.buffer) < c.size {
		c.buffer = append(c.buffer, v)
		return true
	}
	if pair != nil {
		c.blockedSends = append(c.blockedSends, *pair)
	}
	return false
}

func (c *channelImpl) Close() {
	c.closed = true
	for i := 0; i < len(c.blockedReceives); i++ {
		callback := c.blockedReceives[i]
		callback(nil, false)
	}
	// All blocked sends are going to panic
	for i := 0; i < len(c.blockedSends); i++ {
		b := c.blockedSends[i]
		b.callback()
	}
}

// initialYield called at the beginning of the coroutine execution
// stackDepth is the depth of top of the stack to omit when stack trace is generated
// to hide frames internal to the framework.
func (s *coroutineState) initialYield(stackDepth int, status string) {
	keepBlocked := true
	for keepBlocked {
		f := <-s.unblock
		keepBlocked = f(status, stackDepth+1)
	}
}

// yield indicates that coroutine cannot make progress and should sleep
// this call blocks
func (s *coroutineState) yield(status string) {
	s.aboutToBlock <- true
	s.initialYield(3, status) // omit three levels of stack. To adjust change to 0 and count the lines to remove.
	s.keptBlocked = true
}

func getStackTrace(coroutineName, status string, stackDepth int) string {
	top := fmt.Sprintf("coroutine %s [%s]:", coroutineName, status)
	// Omit top stackDepth frames + top status line.
	// Omit bottom two frames which is wrapping of coroutine in a goroutine.
	return getStackTraceRaw(top, stackDepth*2+1, 4)
}

func getStackTraceRaw(top string, omitTop, omitBottom int) string {
	stack := stackBuf[:runtime.Stack(stackBuf[:], false)]
	rawStack := fmt.Sprintf("%s", strings.TrimRightFunc(string(stack), unicode.IsSpace))
	if disableCleanStackTraces {
		return rawStack
	}
	lines := strings.Split(rawStack, "\n")
	lines = lines[omitTop : len(lines)-omitBottom]
	lines = append([]string{top}, lines...)
	return strings.Join(lines, "\n")
}

// unblocked is called by coroutine to indicate that since the last time yield was unblocked channel or select
// where unblocked versus calling yield again after checking their condition
func (s *coroutineState) unblocked() {
	s.keptBlocked = false
}

func (s *coroutineState) call() {
	s.unblock <- func(status string, stackDepth int) bool {
		return false // unblock
	}
	<-s.aboutToBlock
}

func (s *coroutineState) close() {
	s.closed = true
	s.aboutToBlock <- true
}

func (s *coroutineState) exit() {
	if !s.closed {
		s.unblock <- func(status string, stackDepth int) bool {
			runtime.Goexit()
			return true
		}
	}
}

func (s *coroutineState) stackTrace() string {
	if s.closed {
		return ""
	}
	stackCh := make(chan string, 1)
	s.unblock <- func(status string, stackDepth int) bool {
		stackCh <- getStackTrace(s.name, status, stackDepth+2)
		return true
	}
	return <-stackCh
}

func (s *coroutineState) NewCoroutine(ctx Context, f func(ctx Context)) {
	s.dispatcher.newCoroutine(ctx, f)
}

func (s *coroutineState) NewNamedCoroutine(ctx Context, name string, f func(ctx Context)) {
	s.dispatcher.newNamedCoroutine(ctx, name, f)
}

func (s *coroutineState) NewSelector() Selector {
	s.dispatcher.selectorSequence++
	return s.NewNamedSelector(fmt.Sprintf("selector-%v", s.dispatcher.selectorSequence))
}

func (s *coroutineState) NewNamedSelector(name string) Selector {
	return &selectorImpl{name: name}
}

func (s *coroutineState) NewChannel() Channel {
	s.dispatcher.channelSequence++
	return s.NewNamedChannel(fmt.Sprintf("chan-%v", s.dispatcher.channelSequence))
}

func (s *coroutineState) NewNamedChannel(name string) Channel {
	return &channelImpl{name: name}
}

func (s *coroutineState) NewBufferedChannel(size int) Channel {
	return &channelImpl{size: size}
}

func (s *coroutineState) NewNamedBufferedChannel(name string, size int) Channel {
	return &channelImpl{name: name, size: size}
}

func (d *dispatcherImpl) newCoroutine(ctx Context, f func(ctx Context)) {
	d.newNamedCoroutine(ctx, fmt.Sprintf("%v", d.sequence+1), f)
}

func (d *dispatcherImpl) newNamedCoroutine(ctx Context, name string, f func(ctx Context)) {
	state := d.newState(name)
	spawned := WithValue(ctx, coroutinesContextKey, state)
	go func(crt *coroutineState) {
		defer crt.close()
		defer func() {
			if r := recover(); r != nil {
				st := getStackTrace(name, "panic", 4)
				crt.panicError = newPanicError(r, st)
			}
		}()
		crt.initialYield(1, "")
		f(spawned)
	}(state)
}

func (d *dispatcherImpl) newState(name string) *coroutineState {
	c := &coroutineState{
		name:         name,
		dispatcher:   d,
		aboutToBlock: make(chan bool, 1),
		unblock:      make(chan unblockFunc),
	}
	d.sequence++
	d.coroutines = append(d.coroutines, c)
	return c
}

func (d *dispatcherImpl) ExecuteUntilAllBlocked() (err PanicError) {
	d.mutex.Lock()
	if d.closed {
		panic("dispatcher is closed")
	}
	if d.executing {
		panic("call to ExecuteUntilAllBlocked (possibly from a coroutine) while it is already running")
	}
	d.executing = true
	d.mutex.Unlock()
	defer func() { d.executing = false }()
	allBlocked := false
	// Keep executing until at least one goroutine made some progress
	for !allBlocked {
		// Give every coroutine chance to execute removing closed ones
		allBlocked = true
		lastSequence := d.sequence
		for i := 0; i < len(d.coroutines); i++ {
			c := d.coroutines[i]
			if !c.closed {
				// TODO: Support handling of panic in a coroutine by dispatcher.
				// TODO: Dump all outstanding coroutines if one of them panics
				c.call()
			}
			// c.call() can close the context so check again
			if c.closed {
				// remove the closed one from the slice
				d.coroutines = append(d.coroutines[:i],
					d.coroutines[i+1:]...)
				i--
				if c.panicError != nil {
					return c.panicError
				}
				allBlocked = false

			} else {
				allBlocked = allBlocked && (c.keptBlocked || c.closed)
			}
		}
		// Set allBlocked to false if new coroutines where created
		allBlocked = allBlocked && lastSequence == d.sequence
		if len(d.coroutines) == 0 {
			break
		}
	}
	return nil
}

func (d *dispatcherImpl) IsDone() bool {
	return len(d.coroutines) == 0
}

func (d *dispatcherImpl) Close() {
	d.mutex.Lock()
	if d.closed {
		d.mutex.Unlock()
		return
	}
	d.closed = true
	d.mutex.Unlock()
	for i := 0; i < len(d.coroutines); i++ {
		c := d.coroutines[i]
		if !c.closed {
			c.exit()
		}
	}
}

func (d *dispatcherImpl) StackTrace() string {
	var result string
	for i := 0; i < len(d.coroutines); i++ {
		c := d.coroutines[i]
		if !c.closed {
			if len(result) > 0 {
				result += "\n\n"
			}
			result += c.stackTrace()
		}
	}
	return result
}

func (s *selectorImpl) AddReceive(c Channel, f func(v interface{})) Selector {
	s.cases = append(s.cases, &selectCase{channel: c.(*channelImpl), receiveFunc: &f})
	return s
}

func (s *selectorImpl) AddReceiveWithMoreFlag(c Channel, f func(v interface{}, more bool)) Selector {
	s.cases = append(s.cases, &selectCase{channel: c.(*channelImpl), receiveWithMoreFlagFunc: &f})
	return s
}

func (s *selectorImpl) AddSend(c Channel, v interface{}, f func()) Selector {
	s.cases = append(s.cases, &selectCase{channel: c.(*channelImpl), sendFunc: &f, sendValue: &v})
	return s
}

func (s *selectorImpl) AddFuture(future Future, f func(future Future)) Selector {
	asyncF, ok := future.(asyncFuture)
	if !ok {
		panic("cannot chain Future that wasn't created with cadence.NewFuture")
	}
	s.cases = append(s.cases, &selectCase{future: asyncF, futureFunc: &f})
	return s
}

func (s *selectorImpl) AddDefault(f func()) {
	s.defaultFunc = &f
}

func (s *selectorImpl) Select(ctx Context) {
	state := getState(ctx)
	var readyBranch func()
	for _, pair := range s.cases {
		if pair.receiveFunc != nil {
			f := *pair.receiveFunc
			callback := func(v interface{}, more bool) bool {
				if readyBranch != nil {
					return false
				}
				readyBranch = func() {
					f(v)
				}
				return true
			}

			v, ok, more := pair.channel.receiveAsyncImpl(callback)
			if ok || !more {
				f(v)
				return
			}
		} else if pair.receiveWithMoreFlagFunc != nil {
			f := *pair.receiveWithMoreFlagFunc
			callback := func(v interface{}, more bool) bool {
				if readyBranch != nil {
					return false
				}
				readyBranch = func() {
					f(v, more)
				}
				return true
			}
			v, ok, more := pair.channel.receiveAsyncImpl(callback)
			if ok || !more {
				f(v, more)
				return
			}
		} else if pair.sendFunc != nil {
			f := *pair.sendFunc
			p := &valueCallbackPair{
				value: *pair.sendValue,
				callback: func() bool {
					if readyBranch != nil {
						return false
					}
					readyBranch = func() {
						f()
					}
					return true
				},
			}
			ok := pair.channel.sendAsyncImpl(*pair.sendValue, p)
			if ok {
				f()
				return
			}
		} else if pair.futureFunc != nil {
			p := pair
			f := *p.futureFunc
			callback := func(v interface{}, more bool) bool {
				if readyBranch != nil {
					return false
				}
				p.futureFunc = nil
				readyBranch = func() {
					f(p.future)
				}
				return true
			}
			_, ok, _ := p.future.GetAsync(callback)
			if ok {
				p.futureFunc = nil
				f(p.future)
				return
			}
		}
	}
	if s.defaultFunc != nil {
		f := *s.defaultFunc
		f()
		return
	}
	for {
		if readyBranch != nil {
			readyBranch()
			state.unblocked()
			return
		}
		state.yield(fmt.Sprintf("blocked on %s.Select", s.name))
	}
}

// NewWorkflowDefinition creates a  WorkflowDefinition from a Workflow
func newWorkflowDefinition(workflow workflow) workflowDefinition {
	return &syncWorkflowDefinition{workflow: workflow}
}

func getValidatedWorkerFunction(workflowFunc interface{}, args []interface{}) (*WorkflowType, []byte, error) {
	fnName := ""
	fType := reflect.TypeOf(workflowFunc)
	switch fType.Kind() {
	case reflect.String:
		fnName = reflect.ValueOf(workflowFunc).String()

	case reflect.Func:
		if err := validateFunctionArgs(workflowFunc, args, true); err != nil {
			return nil, nil, err
		}
		fnName = getFunctionName(workflowFunc)

	default:
		return nil, nil, fmt.Errorf(
			"Invalid type 'workflowFunc' parameter provided, it can be either worker function or name of the worker type: %v",
			workflowFunc)
	}

	input, err := getHostEnvironment().encodeArgs(args)
	if err != nil {
		return nil, nil, err
	}
	return &WorkflowType{Name: fnName}, input, nil
}

const workflowEnvOptionsContextKey = "wfEnvOptions"

func getWorkflowEnvOptions(ctx Context) *wfEnvironmentOptions {
	env := ctx.Value(workflowEnvOptionsContextKey)
	if env != nil {
		return env.(*wfEnvironmentOptions)
	}
	return nil
}

func setWorkflowEnvOptionsIfNotExist(ctx Context) Context {
	if valCtx := getWorkflowEnvOptions(ctx); valCtx == nil {
		return WithValue(ctx, workflowEnvOptionsContextKey, &wfEnvironmentOptions{})
	}
	return ctx
}

type wfEnvironmentOptions struct {
	workflowType                        *WorkflowType
	input                               []byte
	taskListName                        *string
	executionStartToCloseTimeoutSeconds *int32
	taskStartToCloseTimeoutSeconds      *int32
	domain                              *string
}

// decodeFutureImpl
type decodeFutureImpl struct {
	*futureImpl
	fn interface{}
}

func (d *decodeFutureImpl) Get(ctx Context, value interface{}) error {
	_, more := d.futureImpl.channel.ReceiveWithMoreFlag(ctx)
	if more {
		panic("not closed")
	}
	if !d.futureImpl.ready {
		panic("not ready")
	}
	if value == nil {
		return d.futureImpl.err
	}
	rf := reflect.ValueOf(value)
	if rf.Type().Kind() != reflect.Ptr {
		return errors.New("value parameter is not a pointer")
	}

	err := deSerializeFunctionResult(d.fn, d.futureImpl.value.([]byte), value)
	if err != nil {
		return err
	}
	return d.futureImpl.err
}