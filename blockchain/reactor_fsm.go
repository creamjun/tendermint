package blockchain

import (
	"errors"
	"fmt"
	"time"

	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/types"
)

// Blockchain Reactor State
type bReactorFSMState struct {
	name string

	// called when transitioning out of current state
	handle func(*bReactorFSM, bReactorEvent, bReactorEventData) (next *bReactorFSMState, err error)
	// called when entering the state
	enter func(fsm *bReactorFSM)

	// timeout to ensure FSM is not stuck in a state forever
	// the timer is owned and run by the fsm instance
	timeout time.Duration
}

func (s *bReactorFSMState) String() string {
	return s.name
}

// Blockchain Reactor State Machine
type bReactorFSM struct {
	logger    log.Logger
	startTime time.Time

	state      *bReactorFSMState
	stateTimer *time.Timer
	pool       *blockPool

	// interface used to call the Blockchain reactor to send StatusRequest, BlockRequest, reporting errors, etc.
	toBcR bcRMessageInterface
}

func NewFSM(height int64, toBcR bcRMessageInterface) *bReactorFSM {
	return &bReactorFSM{
		state:     unknown,
		startTime: time.Now(),
		pool:      newBlockPool(height, toBcR),
		toBcR:     toBcR,
	}
}

// bReactorEventData is part of the message sent by the reactor to the FSM and used by the state handlers.
type bReactorEventData struct {
	peerId         p2p.ID
	err            error        // for peer error: timeout, slow; for processed block event if error occurred
	height         int64        // for status response; for processed block event
	block          *types.Block // for block response
	stateName      string       // for state timeout events
	length         int          // for block response event, length of received block, used to detect slow peers
	maxNumRequests int32        // for request needed event, maximum number of pending requests
}

// Blockchain Reactor Events (the input to the state machine)
type bReactorEvent uint

const (
	// message type events
	startFSMEv = iota + 1
	statusResponseEv
	blockResponseEv
	processedBlockEv
	makeRequestsEv
	stopFSMEv

	// other events
	peerRemoveEv = iota + 256
	stateTimeoutEv
)

func (msg *bReactorMessageData) String() string {
	var dataStr string

	switch msg.event {
	case startFSMEv:
		dataStr = ""
	case statusResponseEv:
		dataStr = fmt.Sprintf("peer=%v height=%v", msg.data.peerId, msg.data.height)
	case blockResponseEv:
		dataStr = fmt.Sprintf("peer=%v block.height=%v lenght=%v", msg.data.peerId, msg.data.block.Height, msg.data.length)
	case processedBlockEv:
		dataStr = fmt.Sprintf("error=%v", msg.data.err)
	case makeRequestsEv:
		dataStr = ""
	case stopFSMEv:
		dataStr = ""
	case peerRemoveEv:
		dataStr = fmt.Sprintf("peer: %v is being removed by the switch", msg.data.peerId)
	case stateTimeoutEv:
		dataStr = fmt.Sprintf("state=%v", msg.data.stateName)

	default:
		dataStr = fmt.Sprintf("cannot interpret message data")
		return "event unknown"
	}

	return fmt.Sprintf("%v: %v", msg.event, dataStr)
}

func (ev bReactorEvent) String() string {
	switch ev {
	case startFSMEv:
		return "startFSMEv"
	case statusResponseEv:
		return "statusResponseEv"
	case blockResponseEv:
		return "blockResponseEv"
	case processedBlockEv:
		return "processedBlockEv"
	case makeRequestsEv:
		return "makeRequestsEv"
	case stopFSMEv:
		return "stopFSMEv"
	case peerRemoveEv:
		return "peerRemoveEv"
	case stateTimeoutEv:
		return "stateTimeoutEv"
	default:
		return "event unknown"
	}

}

// states
var (
	unknown      *bReactorFSMState
	waitForPeer  *bReactorFSMState
	waitForBlock *bReactorFSMState
	finished     *bReactorFSMState
)

// state timers
var (
	waitForPeerTimeout                 = 2 * time.Second
	waitForBlockAtCurrentHeightTimeout = 5 * time.Second
)

// errors
var (
	errNoErrorFinished          = errors.New("FSM is finished")
	errInvalidEvent             = errors.New("invalid event in current state")
	errNoPeerResponse           = errors.New("FSM timed out on peer response")
	errBadDataFromPeer          = errors.New("received from wrong peer or bad block")
	errMissingBlocks            = errors.New("missing blocks")
	errBlockVerificationFailure = errors.New("block verification failure, redo")
	errNilPeerForBlockRequest   = errors.New("nil peer for block request")
	errSendQueueFull            = errors.New("block request not made, send-queue is full")
	errPeerTooShort             = errors.New("peer height too low, old peer removed/ new peer not added")
	errSlowPeer                 = errors.New("peer is not sending us data fast enough")
	errSwitchRemovesPeer        = errors.New("switch is removing peer")
	errTimeoutEventWrongState   = errors.New("timeout event for a different state than the current one")
)

func init() {
	unknown = &bReactorFSMState{
		name: "unknown",
		handle: func(fsm *bReactorFSM, ev bReactorEvent, data bReactorEventData) (*bReactorFSMState, error) {
			switch ev {
			case startFSMEv:
				// Broadcast Status message. Currently doesn't return non-nil error.
				fsm.toBcR.sendStatusRequest()
				return waitForPeer, nil

			case stopFSMEv:
				return finished, errNoErrorFinished

			default:
				return unknown, errInvalidEvent
			}
		},
	}

	waitForPeer = &bReactorFSMState{
		name:    "waitForPeer",
		timeout: waitForPeerTimeout,
		enter: func(fsm *bReactorFSM) {
			// Stop when leaving the state.
			fsm.resetStateTimer()
		},
		handle: func(fsm *bReactorFSM, ev bReactorEvent, data bReactorEventData) (*bReactorFSMState, error) {
			switch ev {
			case stateTimeoutEv:
				if data.stateName != "waitForPeer" {
					fsm.logger.Error("received a state timeout event for different state", "state", data.stateName)
					return waitForPeer, errTimeoutEventWrongState
				}
				// There was no statusResponse received from any peer.
				// Should we send status request again?
				return finished, errNoPeerResponse

			case statusResponseEv:
				if err := fsm.pool.updatePeer(data.peerId, data.height); err != nil {
					if len(fsm.pool.peers) == 0 {
						return waitForPeer, err
					}
				}
				return waitForBlock, nil

			case stopFSMEv:
				if fsm.stateTimer != nil {
					fsm.stateTimer.Stop()
				}
				return finished, errNoErrorFinished

			default:
				return waitForPeer, errInvalidEvent
			}
		},
	}

	waitForBlock = &bReactorFSMState{
		name:    "waitForBlock",
		timeout: waitForBlockAtCurrentHeightTimeout,
		enter: func(fsm *bReactorFSM) {
			// Stop when leaving the state.
			fsm.resetStateTimer()
		},
		handle: func(fsm *bReactorFSM, ev bReactorEvent, data bReactorEventData) (*bReactorFSMState, error) {
			switch ev {

			case statusResponseEv:
				err := fsm.pool.updatePeer(data.peerId, data.height)
				if len(fsm.pool.peers) == 0 {
					return waitForPeer, err
				}
				return waitForBlock, err

			case blockResponseEv:
				fsm.logger.Debug("blockResponseEv", "H", data.block.Height)
				err := fsm.pool.addBlock(data.peerId, data.block, data.length)
				if err != nil {
					// A block was received that was unsolicited, from unexpected peer, or that we already have it.
					// Ignore block, remove peer and send error to switch.
					fsm.pool.removePeer(data.peerId, err)
					fsm.toBcR.sendPeerError(err, data.peerId)
				}
				return waitForBlock, err

			case processedBlockEv:
				fsm.logger.Debug("processedBlockEv", "err", data.err)
				first, second, _ := fsm.pool.getNextTwoBlocks()
				if data.err != nil {
					fsm.logger.Error("process blocks returned error", "err", data.err, "first", first.block.Height, "second", second.block.Height)
					fsm.logger.Error("send peer error for", "peer", first.peer.id)
					fsm.toBcR.sendPeerError(data.err, first.peer.id)
					fsm.logger.Error("send peer error for", "peer", second.peer.id)
					fsm.toBcR.sendPeerError(data.err, second.peer.id)
					// Remove the first two blocks. This will also remove the peers
					fsm.pool.invalidateFirstTwoBlocks(data.err)
				} else {
					fsm.pool.processedCurrentHeightBlock()
					if fsm.pool.reachedMaxHeight() {
						fsm.stop()
						return finished, nil
					}
					fsm.resetStateTimer()
				}

				return waitForBlock, data.err

			case peerRemoveEv:
				// This event is sent by the switch to remove disconnected and errored peers.
				fsm.pool.removePeer(data.peerId, data.err)
				return waitForBlock, nil

			case makeRequestsEv:
				fsm.makeNextRequests(data.maxNumRequests)
				return waitForBlock, nil

			case stateTimeoutEv:
				if data.stateName != "waitForBlock" {
					fsm.logger.Error("received a state timeout event for different state", "state", data.stateName)
					return waitForBlock, errTimeoutEventWrongState
				}
				// We haven't received the block at current height. Remove peer.
				fsm.pool.removePeerAtCurrentHeight(errNoPeerResponse)
				fsm.resetStateTimer()
				if len(fsm.pool.peers) == 0 {
					return waitForPeer, errNoPeerResponse
				}
				return waitForBlock, errNoPeerResponse

			case stopFSMEv:
				if fsm.stateTimer != nil {
					fsm.stateTimer.Stop()
				}
				return finished, errNoErrorFinished

			default:
				return waitForBlock, errInvalidEvent
			}
		},
	}

	finished = &bReactorFSMState{
		name: "finished",
		enter: func(fsm *bReactorFSM) {
			fsm.cleanup()
		},
		handle: func(fsm *bReactorFSM, ev bReactorEvent, data bReactorEventData) (*bReactorFSMState, error) {
			return nil, nil
		},
	}
}

// Interface used by FSM for sending Block and Status requests,
// informing of peer errors and state timeouts
// Implemented by BlockchainReactor and tests
type bcRMessageInterface interface {
	sendStatusRequest()
	sendBlockRequest(peerID p2p.ID, height int64) error
	sendPeerError(err error, peerID p2p.ID)
	resetStateTimer(name string, timer **time.Timer, timeout time.Duration)
}

func (fsm *bReactorFSM) setLogger(l log.Logger) {
	fsm.logger = l
	fsm.pool.setLogger(l)
}

// Starts the FSM goroutine.
func (fsm *bReactorFSM) start() {
	_ = fsm.handle(&bReactorMessageData{event: startFSMEv})
}

// Stops the FSM goroutine.
func (fsm *bReactorFSM) stop() {
	_ = fsm.handle(&bReactorMessageData{event: stopFSMEv})
}

// handle processes messages and events sent to the FSM.
func (fsm *bReactorFSM) handle(msg *bReactorMessageData) error {
	fsm.logger.Debug("FSM received", "event", msg, "state", fsm.state)

	if fsm.state == nil {
		fsm.state = unknown
	}
	next, err := fsm.state.handle(fsm, msg.event, msg.data)
	if err != nil {
		fsm.logger.Error("FSM event handler returned", "err", err, "state", fsm.state, "event", msg.event)
	}

	oldState := fsm.state.name
	fsm.transition(next)
	if oldState != fsm.state.name {
		fsm.logger.Info("FSM changed state", "new_state", fsm.state)
	}
	return err
}

func (fsm *bReactorFSM) transition(next *bReactorFSMState) {
	if next == nil {
		return
	}
	if fsm.state != next {
		fsm.state = next
		if next.enter != nil {
			next.enter(fsm)
		}
	}
}

// Called when entering an FSM state in order to detect lack of progress in the state machine.
// Note the use of the 'bcr' interface to facilitate testing without timer expiring.
func (fsm *bReactorFSM) resetStateTimer() {
	fsm.toBcR.resetStateTimer(fsm.state.name, &fsm.stateTimer, fsm.state.timeout)
}

func (fsm *bReactorFSM) isCaughtUp() bool {
	// Some conditions to determine if we're caught up.
	// Ensures we've either received a block or waited some amount of time,
	// and that we're synced to the highest known height.
	// Note we use maxPeerHeight - 1 because to sync block H requires block H+1
	// to verify the LastCommit.
	receivedBlockOrTimedOut := fsm.pool.height > 0 || time.Since(fsm.startTime) > 5*time.Second
	ourChainIsLongestAmongPeers := fsm.pool.maxPeerHeight == 0 || fsm.pool.height >= (fsm.pool.maxPeerHeight-1) || fsm.state == finished
	isCaughtUp := receivedBlockOrTimedOut && ourChainIsLongestAmongPeers

	return isCaughtUp
}

func (fsm *bReactorFSM) makeNextRequests(maxNumPendingRequests int32) {
	fsm.pool.makeNextRequests(maxNumPendingRequests)
}

func (fsm *bReactorFSM) cleanup() {
	fsm.pool.cleanup()
}
