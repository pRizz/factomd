package state

import (
	"container/list"
	"time"

	"github.com/FactomProject/factomd/common/messages"
)

func (list *DBStateList) Catchup() {
	missing := list.State.StatesMissing
	waiting := list.State.StatesWaiting
	recieved := list.State.StatesReceived

	requestTimeout := time.Duration(list.State.RequestTimeout) * time.Second
	requestLimit := list.State.RequestLimit

	// keep the lists up to date with the saved states.
	go func() {
		for {
			// Get information about the known block height
			hs := list.State.GetHighestSavedBlk()
			hk := list.State.GetHighestAck()
			// TODO: find out the significance of highest ack + 2
			if list.State.GetHighestKnownBlock() > hk+2 {
				hk = list.State.GetHighestKnownBlock()
			}

			if recieved.Base() < hs {
				recieved.SetBase(hs)
			}

			// TODO: removing missing and waiting states could be done in parallel.
			// remove any states from the missing list that have been saved.
			for e := missing.List.Front(); e != nil; e = e.Next() {
				s := e.Value.(*MissingState)
				if s.Height() <= recieved.Base() {
					missing.Del(s.Height())
				}
			}

			// remove any states from the waiting list that have been saved.
			for e := waiting.List.Front(); e != nil; e = e.Next() {
				s := e.Value.(*WaitingState)
				if s.Height() <= recieved.Base() {
					waiting.Del(s.Height())
				}
			}

			// find gaps in the recieved list
			for e := recieved.List.Front(); e != nil; e = e.Next() {
				// if the height of the next recieved state is not equal to the
				// height of the current recieved state plus one then there is a
				// gap in the recieved state list.
				n := e.Value.(*ReceivedState).Height()
				if e.Next() != nil {
					for n+1 < e.Next().Value.(*ReceivedState).Height() {
						missing.Notify <- NewMissingState(n + 1)
					}
				}
			}

			// add all known states after the last recieved to the missing list
			for n := recieved.HeighestRecieved() + 1; n < hk; n++ {
				missing.Notify <- NewMissingState(n)
			}

			time.Sleep(5 * time.Second)
		}
	}()

	go func() {
		for {
			// check the waiting list and move any requests that have timed out
			// back into the missing list.
			for e := waiting.List.Front(); e != nil; e = e.Next() {
				s := e.Value.(*WaitingState)
				if s.RequestAge() > requestTimeout {
					waiting.Del(s.Height())
					missing.Notify <- NewMissingState(s.Height())
				}
			}

			time.Sleep(1 * time.Second)
		}
	}()

	// manage the state lists
	go func() {
		for {
			select {
			case s := <-missing.Notify:
				if recieved.Get(s.Height()) == nil {
					if !waiting.Has(s.Height()) {
						missing.Add(s.Height())
					}
				}
			case s := <-waiting.Notify:
				if !waiting.Has(s.Height()) {
					waiting.Add(s.Height())
				}
			case m := <-recieved.Notify:
				s := NewReceivedState(m)
				waiting.Del(s.Height())
				recieved.Add(s.Height(), s.Message())
				// default:
				// 	time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	// request missing states from the network
	go func() {
		for {
			if waiting.Len() < requestLimit {
				s := missing.GetNext()
				if s != nil && !waiting.Has(s.Height()) {
					msg := messages.NewDBStateMissing(list.State, s.Height(), s.Height())
					if msg != nil {
						msg.SendOut(list.State, msg)
						waiting.Notify <- NewWaitingState(s.Height())
					}
				}
			} else {
				time.Sleep(5 * time.Second)
			}
		}
	}()
}

// MissingState is information about a DBState that is known to exist but is not
// available on the current node.
type MissingState struct {
	height uint32
}

// NewMissingState creates a new MissingState for the DBState at a specific
// height.
func NewMissingState(height uint32) *MissingState {
	s := new(MissingState)
	s.height = height
	return s
}

func (s *MissingState) Height() uint32 {
	return s.height
}

// TODO: if StatesMissing takes a long time to seek through the list we should
// replace the iteration with binary search

type StatesMissing struct {
	List   *list.List
	Notify chan *MissingState
}

// NewStatesMissing creates a new list of missing DBStates.
func NewStatesMissing() *StatesMissing {
	l := new(StatesMissing)
	l.List = list.New()
	l.Notify = make(chan *MissingState)
	return l
}

// Add adds a new MissingState to the list.
func (l *StatesMissing) Add(height uint32) {
	for e := l.List.Back(); e != nil; e = e.Prev() {
		s := e.Value.(*MissingState)
		if height > s.Height() {
			l.List.InsertAfter(NewMissingState(height), e)
			return
		} else if height == s.Height() {
			return
		}
	}
	l.List.PushFront(NewMissingState(height))
}

// Del removes a MissingState from the list.
func (l *StatesMissing) Del(height uint32) {
	for e := l.List.Front(); e != nil; e = e.Next() {
		if e.Value.(*MissingState).Height() == height {
			l.List.Remove(e)
			break
		}
	}
}

func (l *StatesMissing) Get(height uint32) *MissingState {
	for e := l.List.Front(); e != nil; e = e.Next() {
		s := e.Value.(*MissingState)
		if s.Height() == height {
			return s
		}
	}
	return nil
}

// GetNext pops the next MissingState from the list.
func (l *StatesMissing) GetNext() *MissingState {
	e := l.List.Front()
	if e != nil {
		s := e.Value.(*MissingState)
		l.List.Remove(e)
		return s
	}
	return nil
}

type WaitingState struct {
	height        uint32
	requestedTime time.Time
}

func NewWaitingState(height uint32) *WaitingState {
	s := new(WaitingState)
	s.height = height
	s.requestedTime = time.Now()
	return s
}

func (s *WaitingState) Height() uint32 {
	return s.height
}

func (s *WaitingState) RequestAge() time.Duration {
	return time.Since(s.requestedTime)
}

func (s *WaitingState) ResetRequestAge() {
	s.requestedTime = time.Now()
}

type StatesWaiting struct {
	List   *list.List
	Notify chan *WaitingState
}

func NewStatesWaiting() *StatesWaiting {
	l := new(StatesWaiting)
	l.List = list.New()
	l.Notify = make(chan *WaitingState)
	return l
}

func (l *StatesWaiting) Add(height uint32) {
	l.List.PushBack(NewWaitingState(height))
}

func (l *StatesWaiting) Del(height uint32) {
	for e := l.List.Front(); e != nil; e = e.Next() {
		s := e.Value.(*WaitingState)
		if s.Height() == height {
			l.List.Remove(e)
		}
	}
}

func (l *StatesWaiting) Get(height uint32) *WaitingState {
	for e := l.List.Front(); e != nil; e = e.Next() {
		s := e.Value.(*WaitingState)
		if s.Height() == height {
			return s
		}
	}
	return nil
}

func (l *StatesWaiting) Has(height uint32) bool {
	for e := l.List.Front(); e != nil; e = e.Next() {
		s := e.Value.(*WaitingState)
		if s.Height() == height {
			return true
		}
	}
	return false
}

func (l *StatesWaiting) Len() int {
	return l.List.Len()
}

// ReceivedState represents a DBStateMsg received from the network
type ReceivedState struct {
	height uint32
	msg    *messages.DBStateMsg
}

// NewReceivedState creates a new member for the StatesReceived list
func NewReceivedState(msg *messages.DBStateMsg) *ReceivedState {
	s := new(ReceivedState)
	s.height = msg.DirectoryBlock.GetHeader().GetDBHeight()
	s.msg = msg
	return s
}

// Height returns the block height of the received state
func (s *ReceivedState) Height() uint32 {
	return s.height
}

// Message returns the DBStateMsg received from the network.
func (s *ReceivedState) Message() *messages.DBStateMsg {
	return s.msg
}

// StatesReceived is the list of DBStates recieved from the network. "base"
// represents the height of known saved states.
type StatesReceived struct {
	List   *list.List
	Notify chan *messages.DBStateMsg
	base   uint32
}

func NewStatesReceived() *StatesReceived {
	l := new(StatesReceived)
	l.List = list.New()
	l.Notify = make(chan *messages.DBStateMsg)
	return l
}

// Base returns the base height of the StatesReceived list
func (l *StatesReceived) Base() uint32 {
	return l.base
}

func (l *StatesReceived) SetBase(height uint32) {
	l.base = height

	for e := l.List.Front(); e != nil; e = e.Next() {
		switch v := e.Value.(*ReceivedState).Height(); {
		case v < l.base:
			l.List.Remove(e)
		case v == l.base:
			l.List.Remove(e)
			break
		case v > l.base:
			break
		}
	}
}

// HeighestRecieved returns the height of the last member in StatesReceived
func (l *StatesReceived) HeighestRecieved() uint32 {
	height := uint32(0)
	s := l.List.Back()
	if s != nil {
		height = s.Value.(*ReceivedState).Height()
	}
	if l.Base() > height {
		return l.Base()
	}
	return height
}

// Add adds a new recieved state to the list.
func (l *StatesReceived) Add(height uint32, msg *messages.DBStateMsg) {
	for e := l.List.Back(); e != nil; e = e.Prev() {
		s := e.Value.(*ReceivedState)
		if height > s.Height() {
			l.List.InsertAfter(NewReceivedState(msg), e)
			return
		} else if height == s.Height() {
			return
		}
	}
	l.List.PushFront(NewReceivedState(msg))
}

// Del removes a state from the StatesReceived list
func (l *StatesReceived) Del(height uint32) {
	for e := l.List.Back(); e != nil; e = e.Prev() {
		if e.Value.(*ReceivedState).Height() == height {
			l.List.Remove(e)
			break
		}
	}
}

// Get returns a member from the StatesReceived list
func (l *StatesReceived) Get(height uint32) *ReceivedState {
	for e := l.List.Back(); e != nil; e = e.Prev() {
		if e.Value.(*ReceivedState).Height() == height {
			return e.Value.(*ReceivedState)
		}
	}
	return nil
}

func (l *StatesReceived) GetNext() *ReceivedState {
	e := l.List.Front()
	if e != nil {
		s := e.Value.(*ReceivedState)
		if s.Height() == l.Base()+1 {
			l.SetBase(s.Height())
			l.List.Remove(e)
			return s
		}
		if s.Height() <= l.Base() {
			l.List.Remove(e)
		}
	}
	return nil
}
