package channel

import (
	"sync"

	"github.com/goose-lang/primitive"
)

type ChannelState uint64

const (
	// Only start and closed used for buffered channels.
	start          ChannelState = 0
	receiver_ready ChannelState = 1
	sender_ready   ChannelState = 2
	receiver_done  ChannelState = 3
	sender_done    ChannelState = 4
	closed         ChannelState = 5
)

type Channel[T any] struct {
	lock  *sync.Mutex
	state ChannelState

	// Values only used for buffered channels. The length of buffer being 0 is how we determine a
	// channel is unbuffered. buffer is a circular queue that represents the channel's buffer
	buffer []T
	first  uint64
	count  uint64

	// Value only used for unbuffered channels
	v T
}

// buffer_size = 0 is an unbuffered channel
func NewChannelRef[T any](buffer_size uint64) *Channel[T] {
	return &Channel[T]{
		buffer: make([]T, buffer_size),
		lock:   new(sync.Mutex),
		first:  0,
		count:  0,
		state:  start,
	}
}

// c.Send(val)
//
// is equivalent to:
//
// c <- val
func (c *Channel[T]) Send(val T) {
	if c == nil {
		// Block forever
		for {
		}
	}

	// Create a send case for this channel
	sendCase := NewSendCase(c, val)

	// Run a blocking select with just this one case
	// This will block until the send succeeds
	Select1(sendCase, true)
	return
}

// Equivalent to:
// value, ok := <-c
// Notably, this requires the user to consume the ok bool which is not actually required with Go
// channels. This should be able to be solved by adding an overload wrapper that discards the ok
// bool.
func (c *Channel[T]) Receive() (T, bool) {
	if c == nil {
		// Block forever
		for {
		}
	}

	// Create a receive case for this channel
	recvCase := NewRecvCase(c)

	// Run a blocking select with just this one case
	// This will block until the receive succeeds
	Select1(recvCase, true)

	return recvCase.Value, recvCase.Ok
}

// This is a non-blocking attempt at closing. The only reason close blocks ever is because there
// may be successful exchanges that need to complete, which is equivalent to the go runtime where
// the closer must still obtain the channel's lock
func (c *Channel[T]) TryClose() bool {
	if c.state == closed {
		panic("close of closed channel")
	}
	// For unbuffered channels, if there is an exchange in progress, let the exchange complete.
	// In the real channel code the lock is held while this happens.
	if c.state != receiver_done && c.state != sender_done {
		c.state = closed
		return true
	}
	return false
}

// c.Close()
//
// is equivalent to:
//
// close(c)
func (c *Channel[T]) Close() {
	if c == nil {
		panic("close of nil channel")
	}
	var done bool = false
	for !done {
		c.lock.Lock()
		done = c.TryClose()
		c.lock.Unlock()
	}
}

// v := c.ReceiveDiscardOk
//
// is equivalent to:
// v := c<-
// It seems like Go requires ignored return values to be annotated with _ but channels don't
// require this so this will need to be translated.
func (c *Channel[T]) ReceiveDiscardOk() T {
	var return_val T
	return_val, _ = c.Receive()
	return return_val
}

// If there is a value available in the buffer, consume it, otherwise, don't select.
func (c *Channel[T]) BufferedTryReceiveLocked() (bool, T, bool) {
	var v T
	if c.count > 0 {
		c.v = c.buffer[c.first]
		c.first = (c.first + 1) % uint64(len(c.buffer))
		c.count -= 1
		return true, c.v, true
	}
	if c.state == closed {
		return true, v, false
	}
	return false, v, true
}

func (c *Channel[T]) BufferedTryReceive() (bool, T, bool) {
	c.lock.Lock()
	selected, return_val, ok := c.BufferedTryReceiveLocked()
	c.lock.Unlock()
	return selected, return_val, ok
}

type OfferResult uint64

const (
	OfferRescinded        OfferResult = 0 // Offer was rescinded (other party didn't arrive in time)
	CompletedExchange     OfferResult = 1 // Other party responded to our offer
	CloseInterruptedOffer OfferResult = 2 // Unexpected state, indicates model bugs.
)

func (c *Channel[T]) UnbufferedTryReceive(blocking bool) (bool, T, bool) {
	var local_val T
	// First critical section: determine state and get value if sender is ready
	c.lock.Lock()
	// No exchange in progress, make an offer, which will "lock" the channel from other
	// receivers since they will do nothing in this function if receiver_ready is observed.
	if c.state == closed {
		c.lock.Unlock()
		return true, local_val, false
	}
	if c.state == sender_ready {
		local_val = c.v
		c.state = receiver_done
		c.lock.Unlock()
		return true, local_val, true
	}
	if c.state == sender_done || c.state == receiver_ready || c.state == receiver_done || !blocking {
		c.lock.Unlock()
		return false, local_val, true
	}
	if c.state == start {
		c.state = receiver_ready
		c.lock.Unlock()
		c.lock.Lock()
		if c.state == closed {
			c.lock.Unlock()
			return true, local_val, false
		}
		// Offer wasn't accepted in time, rescind it.
		if c.state == receiver_ready {
			c.state = start
			c.lock.Unlock()
			return false, local_val, true
		}
		// Offer was accepted, complete the exchange.
		if c.state == sender_done {
			c.state = start
			local_val = c.v
			c.lock.Unlock()
			return true, local_val, true
		}
		// Cases should be exhaustive which is non-obvious here, since close can rescind the offer
		// for us but other receivers cannot.
		panic("not supposed to be here!")
	}
	// We should be exhaustively handling these cases but Go wants a return everywhere
	panic("not supposed to be here!")
}

// Non-blocking receive function used for select statements. Blocking receive is modeled as
// a single blocking select statement which amounts to a for loop until selected.
// The blocking parameter here is used to determine whether or not we will make an offer to a
// waiting sender. If true, we will make an offer since blocking receive is modeled as a for loop
// around nonblocking TryReceive. If false, we don't make an offer since we don't need to match
// with another non-blocking send.
func (c *Channel[T]) TryReceive(blocking bool) (bool, T, bool) {
	if uint64(len(c.buffer)) > 0 {
		return c.BufferedTryReceive()
	} else {
		return c.UnbufferedTryReceive(blocking)
	}
}

type SenderState uint64

const (
	SenderCompletedWithReceiver SenderState = 0 // Sender found a waiting receiver
	SenderMadeOffer             SenderState = 1 // Sender made an offer (no receiver waiting)
	SenderCannotProceed         SenderState = 2 // Exchange in progress, don't select
)

func (c *Channel[T]) SenderCompleteOrOffer(val T, blocking bool) SenderState {
	if c.state == closed {
		panic("send on closed channel")
	}
	// Receiver waiting, complete exchange.
	if c.state == receiver_ready {
		c.state = sender_done
		c.v = val
		return SenderCompletedWithReceiver
	}
	// No exchange in progress, make an offer.
	// Make an offer only if blocking.
	if c.state == start && blocking {
		c.state = sender_ready
		// Save the value in case the receiver completes the exchange.
		c.v = val
		return SenderMadeOffer
	}
	// Exchange in progress, don't select.
	return SenderCannotProceed
}

func (c *Channel[T]) SenderCheckOfferResult() OfferResult {
	if c.state == closed {
		panic("send on closed channel")
	}
	// Receiver accepted offer, complete exchange.
	if c.state == receiver_done {
		c.state = start
		return CompletedExchange
	}
	// Offer still stands, rescind it.
	if c.state == sender_ready {
		c.state = start
		return OfferRescinded
	}
	panic("Invalid state transition with open receive offer")
}

// If the buffer has free space, push our value.
func (c *Channel[T]) BufferedTrySend(val T) bool {
	if c.state == closed {
		panic("send on closed channel")
	}

	// If we have room, buffer our value
	if c.count < uint64(len(c.buffer)) {
		var last uint64 = (c.first + c.count) % uint64(len(c.buffer))
		c.buffer[last] = val
		c.count += 1
		return true
	}
	return false
}

// Non-Blocking send operation for select statements. Blocking send and blocking select
// statements simply call this in a for loop until it returns true.
func (c *Channel[T]) TrySend(val T, blocking bool) bool {
	var buffer_size uint64 = uint64(len(c.buffer))

	// Buffered channel:
	if buffer_size != 0 {
		c.lock.Lock()
		sendResult := c.BufferedTrySend(val)
		c.lock.Unlock()
		return sendResult
	}

	// Unbuffered channel:
	// First critical section: Try to complete send or make offer
	c.lock.Lock()
	senderState := c.SenderCompleteOrOffer(val, blocking)
	c.lock.Unlock()

	// Second critical section: Handle offer case if needed
	if senderState == SenderMadeOffer {
		c.lock.Lock()
		offerResult := c.SenderCheckOfferResult()
		c.lock.Unlock()
		return offerResult == CompletedExchange
	}

	// If we didn't make an offer, we either selected or an exchange is in progress so we bail.
	return senderState == SenderCompletedWithReceiver
}

// c.Len()
//
// is equivalent to:
// len(c)
//
// This might not be worth specifying since it is hard to make good use of channel length
// semantics.
func (c *Channel[T]) Len() uint64 {
	if c == nil {
		return 0
	}
	var chan_len uint64 = 0
	c.lock.Lock()
	chan_len = c.count
	c.lock.Unlock()
	return chan_len
}

// c.Cap()
//
// is equivalent to:
// cap(c)
func (c *Channel[T]) Cap() uint64 {
	if c == nil {
		return 0
	}
	return uint64(len(c.buffer))
}

// The code below models select statements in a similar way to the reflect package's
// dynamic select statements. See unit tests in channel_test.go for examples of
// the intended translation.
type SelectDir uint64

const (
	SelectSend SelectDir = 0 // case Chan <- Send
	SelectRecv SelectDir = 1 // case <-Chan:
)

// value is used for the value the sender will send and also used to return the received value by
// reference.
type SelectCase[T any] struct {
	channel *Channel[T]
	dir     SelectDir
	Value   T
	Ok      bool
}

func NewSendCase[T any](channel *Channel[T], value T) *SelectCase[T] {
	return &SelectCase[T]{
		channel: channel,
		dir:     SelectSend,
		Value:   value,
	}
}

func NewRecvCase[T any](channel *Channel[T]) *SelectCase[T] {
	return &SelectCase[T]{
		channel: channel,
		dir:     SelectRecv,
	}
}

// Uses the applicable Try<Operation> function on the select case's channel. Default is always
// selectable so simply returns true.
func TrySelect[T any](select_case *SelectCase[T], blocking bool) bool {
	var channel *Channel[T] = select_case.channel
	if channel == nil {
		return false
	}
	if select_case.dir == SelectSend {
		return channel.TrySend(select_case.Value, blocking)
	}
	if select_case.dir == SelectRecv {
		var item T
		var ok bool
		var selected bool
		selected, item, ok = channel.TryReceive(blocking)
		// We can use these values for return by reference and they will be implicitly kept alive
		// by the garbage collector so we can use value here for both the send and receive
		// variants. What a miracle it is to not be using C++.
		select_case.Value = item
		select_case.Ok = ok
		return selected

	}
	return false
}

// Select1 performs a select operation on 1 case. This is used for Send and
// Receive as well, since these channel operations in Go are equivalent to
// a single case select statement with no default.
func Select1[T1 any](
	case1 *SelectCase[T1],
	blocking bool) bool {
	var selected bool
	for {
		selected = TrySelect(case1, blocking)
		if selected || !blocking {
			break
		}
	}
	return selected
}

func TrySelectCase2[T1, T2 any](
	index uint64,
	case1 *SelectCase[T1],
	case2 *SelectCase[T2], blocking bool) bool {
	if index == 0 {
		return TrySelect(case1, blocking)
	}
	if index == 1 {
		return TrySelect(case2, blocking)
	}
	panic("index needs to be 0 or 1")
}

func Select2[T1, T2 any](
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	blocking bool) uint64 {

	i := primitive.RandomUint64() % uint64(2)
	if TrySelectCase2(i, case1, case2, blocking) {
		return i
	}

	// If nothing was selected and we're blocking, try in a loop
	for {
		if TrySelect(case1, blocking) {
			return 0
		}
		if TrySelect(case2, blocking) {
			return 1
		}
		if !blocking {
			return 2
		}
	}
}

func TrySelectCase3[T1, T2, T3 any](
	index uint64,
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	case3 *SelectCase[T3], blocking bool) bool {
	if index == 0 {
		return TrySelect(case1, blocking)
	}
	if index == 1 {
		return TrySelect(case2, blocking)
	}
	if index == 2 {
		return TrySelect(case3, blocking)
	}
	panic("index needs to be 0, 1 or 2")
}

func Select3[T1, T2, T3 any](
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	case3 *SelectCase[T3],
	blocking bool) uint64 {

	i := primitive.RandomUint64() % uint64(3)
	if TrySelectCase3(i, case1, case2, case3, blocking) {
		return i
	}

	for {
		if TrySelect(case1, blocking) {
			return 0
		}
		if TrySelect(case2, blocking) {
			return 1
		}
		if TrySelect(case3, blocking) {
			return 2
		}
		if !blocking {
			return 3
		}
	}
}

func TrySelectCase4[T1, T2, T3, T4 any](
	index uint64,
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	case3 *SelectCase[T3],
	case4 *SelectCase[T4], blocking bool) bool {
	if index == 0 {
		return TrySelect(case1, blocking)
	}
	if index == 1 {
		return TrySelect(case2, blocking)
	}
	if index == 2 {
		return TrySelect(case3, blocking)
	}
	if index == 3 {
		return TrySelect(case4, blocking)
	}
	panic("index needs to be 0, 1, 2 or 3")
}

func Select4[T1, T2, T3, T4 any](
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	case3 *SelectCase[T3],
	case4 *SelectCase[T4],
	blocking bool) uint64 {

	i := primitive.RandomUint64() % uint64(4)
	if TrySelectCase4(i, case1, case2, case3, case4, blocking) {
		return i
	}

	for {
		if TrySelect(case1, blocking) {
			return 0
		}
		if TrySelect(case2, blocking) {
			return 1
		}
		if TrySelect(case3, blocking) {
			return 2
		}
		if TrySelect(case4, blocking) {
			return 3
		}
		if !blocking {
			return 4
		}
	}
}

func TrySelectCase5[T1, T2, T3, T4, T5 any](
	index uint64,
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	case3 *SelectCase[T3],
	case4 *SelectCase[T4],
	case5 *SelectCase[T5], blocking bool) bool {
	if index == 0 {
		return TrySelect(case1, blocking)
	}
	if index == 1 {
		return TrySelect(case2, blocking)
	}
	if index == 2 {
		return TrySelect(case3, blocking)
	}
	if index == 3 {
		return TrySelect(case4, blocking)
	}
	if index == 4 {
		return TrySelect(case5, blocking)
	}
	panic("index needs to be 0, 1, 2, 3 or 4")
}

func Select5[T1, T2, T3, T4, T5 any](
	case1 *SelectCase[T1],
	case2 *SelectCase[T2],
	case3 *SelectCase[T3],
	case4 *SelectCase[T4],
	case5 *SelectCase[T5],
	blocking bool) uint64 {

	i := primitive.RandomUint64() % uint64(5)
	if TrySelectCase5(i, case1, case2, case3, case4, case5, blocking) {
		return i
	}

	for {
		if TrySelect(case1, blocking) {
			return 0
		}
		if TrySelect(case2, blocking) {
			return 1
		}
		if TrySelect(case3, blocking) {
			return 2
		}
		if TrySelect(case4, blocking) {
			return 3
		}
		if TrySelect(case5, blocking) {
			return 4
		}
		if !blocking {
			return 5
		}
	}
}
