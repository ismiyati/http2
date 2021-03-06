package http2

import (
	"errors"
	"fmt"
	"sync/atomic"
)

type StreamState int32

const (
	StateIdle StreamState = iota
	StateReservedLocal
	StateReservedRemote
	StateOpen
	StateHalfClosedLocal
	StateHalfClosedRemote
	StateClosed
)

const defaultWeight = 16

type stream struct {
	conn *Conn

	id    uint32
	state StreamState

	weight   uint8
	parent   *stream
	children map[uint32]*stream

	recvFlow *flowController
	sendFlow *remoteFlowController

	Frame
	lastWritten FrameType
	sawEOS      bool

	resetSent,
	resetReceived bool

	wio     chan struct{}
	werr    chan error
	closeCh chan struct{}
}

func (s *stream) active() bool {
	switch StreamState(atomic.LoadInt32((*int32)(&s.state))) {
	case StateOpen, StateHalfClosedLocal, StateHalfClosedRemote:
		return true
	default:
		return false
	}
}

func (s *stream) readable() bool {
	switch StreamState(atomic.LoadInt32((*int32)(&s.state))) {
	case StateOpen, StateHalfClosedLocal:
		return true
	default:
		return false
	}
}

func (s *stream) writable() bool {
	switch StreamState(atomic.LoadInt32((*int32)(&s.state))) {
	case StateOpen, StateHalfClosedRemote:
		return true
	default:
		return false
	}
}

var errStreamClosed = errors.New("stream closed")

func (s *stream) write(frame Frame) error {
	select {
	case <-s.conn.closeCh:
		return ErrClosed
	case <-s.closeCh:
		return errStreamClosed
	case <-s.wio:
		defer func() { s.wio <- struct{}{} }()

		if s.sawEOS {
			return errStreamClosed
		}

		if frame.Type() == FrameHeaders {
			s.Frame = frame
			s.conn.writeQueue.add(s, false)
			return <-s.werr
		}

		data, ok := frame.(*DataFrame)
		if !ok {
			return fmt.Errorf("bad flow control frame type %s", frame.Type())
		}

		dataLen := data.DataLen
		padLen := int(data.PadLen)
		allowed, err := allocateBytes(s, dataLen+padLen)
		if err != nil {
			return err
		}

		if allowed == dataLen+padLen {
			s.Frame = frame
			s.conn.writeQueue.add(s, false)
			return <-s.werr
		}

		chunk := new(DataFrame)
		*chunk = *data
		s.Frame = chunk

		lastFrame := false
		padding := 0

	again:
		chunk.DataLen = dataLen
		if chunk.DataLen > allowed {
			chunk.DataLen = allowed
		}

		padding = allowed - chunk.DataLen
		if padding > padLen {
			padding = padLen
		}

		dataLen -= chunk.DataLen
		padLen -= padding
		lastFrame = dataLen+padLen == 0

		chunk.PadLen = uint8(padding)
		chunk.EndStream = data.EndStream && lastFrame

		s.conn.writeQueue.add(s, false)
		err = <-s.werr

		if lastFrame || err != nil {
			return err
		}

		allowed, err = allocateBytes(s, dataLen+padLen)
		if err != nil {
			return err
		}

		goto again
	}
}

func (s *stream) writeTo(w *frameWriter) error {
	err := s.Frame.(frameWriterTo).writeTo(w)
	s.lastWritten = s.Frame.Type()
	s.sawEOS = s.Frame.EndOfStream()
	s.werr <- err
	if s.sawEOS && err == nil {
		_, err = s.transition(false, s.lastWritten, true)
	}
	return err
}

func (s *stream) cancel(err error) error {
	select {
	case s.werr <- err:
	default:
	}
	return nil
}

func (s *stream) close() {
	for {
		from := StreamState(atomic.LoadInt32((*int32)(&s.state)))
		if s.compareAndSwapState(from, StateClosed) {
			return
		}
	}
}

func (s *stream) local() bool {
	return s.conn.server == ((s.id & 1) == 0)
}

func (s *stream) setPriority(priority Priority) error {
	return nil
}

func (s *stream) compareAndSwapState(from, to StreamState) bool {
	if atomic.CompareAndSwapInt32((*int32)(&s.state), int32(from), int32(to)) {
		switch to {
		case StateReservedLocal, StateReservedRemote:
			if from == StateIdle {
				s.conn.addStream(s)
			}
		case StateOpen, StateHalfClosedLocal, StateHalfClosedRemote:
			switch from {
			case StateIdle, StateReservedLocal, StateReservedRemote:
				if s.local() {
					atomic.AddUint32(&s.conn.numStreams, 1)
				} else {
					atomic.AddUint32(&s.conn.remote.numStreams, 1)
				}

				w := int(s.conn.Settings().InitialWindowSize())
				s.recvFlow = &flowController{s: s, win: w, winUpperBound: w, processedWin: w}

				if to != StateHalfClosedLocal {
					w = int(s.conn.RemoteSettings().InitialWindowSize())
					s.sendFlow = &remoteFlowController{s: s, winCh: make(chan int, 1)}
					s.sendFlow.incrementInitialWindow(w)
				}

				if from == StateIdle {
					s.conn.addStream(s)
				}
			case StateOpen:
				if to == StateHalfClosedLocal {
					s.cancel(errStreamClosed)
					s.sendFlow.cancel()
					s.sendFlow.incrementWindow(-s.sendFlow.window())
				}
			}
		case StateClosed:
			switch from {
			case StateOpen, StateHalfClosedLocal, StateHalfClosedRemote:
				if s.local() {
					atomic.AddUint32(&s.conn.numStreams, ^uint32(0))
				} else {
					atomic.AddUint32(&s.conn.remote.numStreams, ^uint32(0))
				}
			}

			if from != StateClosed {
				close(s.closeCh)

				s.cancel(errStreamClosed)
				if s.sendFlow != nil {
					s.sendFlow.cancel()
					s.sendFlow.incrementWindow(-s.sendFlow.window())
				}
				if s.recvFlow != nil {
					s.recvFlow.returnBytes(s.recvFlow.consumedBytes())
				}

				s.conn.removeStream(s)
			}
		}
		return true
	}
	return false
}

func (s *stream) transition(recv bool, frameType FrameType, endStream bool) (StreamState, error) {
	for {
		from := StreamState(atomic.LoadInt32((*int32)(&s.state)))
		to, ok := from.transition(recv, frameType, endStream)

		if !ok {
			if !recv {
				if from == StateClosed {

					// if frameType == FrameRSTStream {
					// 	return from, ignoreFrame
					// }

					// An endpoint MUST NOT send frames other than PRIORITY on a closed stream.
					return from, fmt.Errorf("stream %d already closed", s.id)
				}
				return from, fmt.Errorf("bad stream state %s", s.state)
			}

			// An endpoint that receives any frame other than PRIORITY
			// after receiving a RST_STREAM MUST treat that as a stream error
			// (Section 5.4.2) of type STREAM_CLOSED.
			if s.resetReceived {
				return from, StreamError{fmt.Errorf("stream %d already closed", s.id), ErrCodeStreamClosed, s.id}
			}

			if s.resetSent {
				// An endpoint MUST ignore frames that it
				// receives on closed streams after it has sent a RST_STREAM frame.
				// An endpoint MAY choose to limit the period over which it ignores
				// frames and treat frames that arrive after this time as being in error.

				// if time.Since(s.closed) <= time.Duration(5)*time.Second {
				// 	return from, ignoreFrame
				// }

				return from, StreamError{fmt.Errorf("stream %d already closed", s.id), ErrCodeStreamClosed, s.id}
			}

			switch from {
			case StateHalfClosedRemote:
				// An endpoint that receives any frames after receiving a frame with the
				// END_STREAM flag set MUST treat that as a connection error
				// (Section 5.4.1) of type STREAM_CLOSED.
				return from, ConnError{fmt.Errorf("stream %d already closed", s.id), ErrCodeStreamClosed}
			case StateClosed:
				// WINDOW_UPDATE or RST_STREAM frames can be received in this state
				// for a short period after a DATA or HEADERS frame containing an
				// END_STREAM flag is sent.  Until the remote peer receives and
				// processes RST_STREAM or the frame bearing the END_STREAM flag, it
				// might send frames of these types.  Endpoints MUST ignore
				// WINDOW_UPDATE or RST_STREAM frames received in this state, though
				// endpoints MAY choose to treat frames that arrive a significant
				// time after sending END_STREAM as a connection error
				// (Section 5.4.1) of type PROTOCOL_ERROR.
				switch frameType {
				case FrameRSTStream, FrameWindowUpdate:

					// if time.Since(s.closed) <= time.Duration(5)*time.Second {
					// 	return from, ignoreFrame
					// }

					return from, ConnError{fmt.Errorf("stream %d already closed", s.id), ErrCodeProtocol}
				}
			}
			return from, ConnError{fmt.Errorf("bad stream state %s", s.state), ErrCodeProtocol}
		}

		if s.compareAndSwapState(from, to) {
			if to == StateClosed && frameType == FrameRSTStream {
				if recv {
					s.resetReceived = true
				} else {
					s.resetSent = true
				}
			}
			return to, nil
		}
	}
}

func (from StreamState) transition(recv bool, frameType FrameType, endStream bool) (to StreamState, ok bool) {
	to = from
	if recv {
		switch from {
		case StateIdle:
			switch frameType {
			case FrameHeaders:
				to = StateOpen
			case FramePriority:
			case FramePushPromise:
				to = StateReservedRemote
			default:
				return
			}
		case StateReservedLocal, StateHalfClosedRemote:
			switch frameType {
			case FramePriority, FrameWindowUpdate:
			case FrameRSTStream:
				to = StateClosed
			default:
				return
			}
		case StateReservedRemote:
			switch frameType {
			case FrameHeaders:
				to = StateHalfClosedLocal
			case FramePriority:
			case FrameRSTStream:
				to = StateClosed
			default:
				return
			}
		case StateOpen, StateHalfClosedLocal:
			switch frameType {
			case FrameRSTStream:
				to = StateClosed
			}
		case StateClosed:
			switch frameType {
			case FramePriority:
			default:
				return
			}
		}
	} else {
		switch from {
		case StateIdle:
			switch frameType {
			case FrameHeaders:
				to = StateOpen
			case FramePriority:
			case FramePushPromise:
				to = StateReservedLocal
			default:
				return
			}
		case StateReservedLocal:
			switch frameType {
			case FrameHeaders:
				to = StateHalfClosedRemote
			case FramePriority:
			case FrameRSTStream:
				to = StateClosed
			default:
				return
			}
		case StateReservedRemote, StateHalfClosedLocal:
			switch frameType {
			case FramePriority, FrameWindowUpdate:
			case FrameRSTStream:
				to = StateClosed
			default:
				return
			}
		case StateOpen:
			switch frameType {
			case FrameRSTStream:
				to = StateClosed
			}
		case StateHalfClosedRemote:
			switch frameType {
			case FrameData, FrameHeaders, FramePriority:
			case FrameRSTStream:
				to = StateClosed
			default:
				return
			}
		case StateClosed:
			switch frameType {
			case FramePriority:
			default:
				return
			}
		}
	}

	ok = true

	if endStream {
		switch to {
		case StateOpen:
			if recv {
				to = StateHalfClosedRemote
			} else {
				to = StateHalfClosedLocal
			}
		case StateHalfClosedLocal, StateHalfClosedRemote:
			to = StateClosed
		}
	}

	return
}
