package consensus

import (
	"bytes"
	"github.com/iotaledger/wasp/packages/committee"
	"github.com/iotaledger/wasp/packages/hashing"
	"github.com/iotaledger/wasp/packages/registry"
	"github.com/iotaledger/wasp/packages/sctransaction"
	"github.com/iotaledger/wasp/packages/state"
	"github.com/iotaledger/wasp/plugins/nodeconn"
	"time"
)

const getBalancesTimeout = 1 * time.Second

func (op *operator) takeAction() {
	op.doLeader()
	op.doSubordinate()
}

func (op *operator) doSubordinate() {
	for _, cr := range op.currentStateCompRequests {
		if cr.processed {
			continue
		}
		if cr.req.reqMsg == nil {
			continue
		}
		cr.processed = true
		//go op.processRequest(cr.reqs, cr.ts, cr.leaderPeerIndex)
	}
}

func (op *operator) doLeader() {
	if op.iAmCurrentLeader() {
		if op.balances == nil {
			// of balances are not known yet, request it from the node
			op.requestBalancesFromNode()
		} else {
			op.startProcessing()
		}
	}
	op.checkQuorum()
}

func (op *operator) requestBalancesFromNode() {
	if op.balances == nil && time.Now().After(op.getBalancesDeadline) {
		addr := op.committee.Address()
		nodeconn.RequestBalancesFromNode(&addr)
		op.getBalancesDeadline = time.Now().Add(getBalancesTimeout)
	}
}

func (op *operator) startProcessing() {
	if op.balances == nil {
		// shouldn't be
		return
	}
	if op.leaderStatus != nil {
		// request already selected and calculations initialized
		return
	}
	reqs := op.selectRequestsToProcess()
	reqIds := takeIds(reqs)
	if len(reqs) == 0 {
		// can't select request to process
		op.log.Debugf("can't select request to process")
		return
	}
	op.log.Debugw("requests selected to process",
		"stateIdx", op.stateTx.MustState().StateIndex(),
		"batch size", len(reqs),
	)
	msgData := hashing.MustBytes(&committee.StartProcessingReqMsg{
		PeerMsgHeader: committee.PeerMsgHeader{
			// ts is set by SendMsgToPeers
			StateIndex: op.stateTx.MustState().StateIndex(),
		},
		RewardAddress: *registry.GetRewardAddress(op.committee.Address()),
		Balances:      op.balances,
		RequestIds:    reqIds,
	})

	numSucc, ts := op.committee.SendMsgToPeers(committee.MsgStartProcessingRequest, msgData)

	op.log.Debugf("%d 'msgStartProcessingRequest' messages sent to peers", numSucc)

	if numSucc < op.quorum()-1 {
		// doesn't make sense to continue because less than quorum sends succeeded
		op.log.Errorf("only %d 'msgStartProcessingRequest' sends succeeded", numSucc)
		return
	}
	var buf bytes.Buffer
	for i := range reqIds {
		buf.Write(reqIds[i][:])
	}
	reqMsgs, ok := takeMsgs(reqs)
	if !ok {
		panic("some req messages are nil")
	}
	op.leaderStatus = &leaderStatus{
		reqs:          reqs,
		batchHash:     sctransaction.BatchHash(reqIds),
		ts:            ts,
		signedResults: make([]*signedResult, op.committee.Size()),
	}
	op.log.Debugf("msgStartProcessingRequest successfully sent to %d peers", numSucc)

	go op.processRequest(runCalculationsParams{
		reqs:            reqMsgs,
		ts:              ts,
		balances:        op.balances,
		rewardAddress:   *registry.GetRewardAddress(op.committee.Address()),
		leaderPeerIndex: op.committee.OwnPeerIndex(),
	})
}

func (op *operator) checkQuorum() bool {
	op.log.Debug("checkQuorum")
	if op.leaderStatus == nil || op.leaderStatus.resultTx == nil || op.leaderStatus.finalized {
		//log.Debug("checkQuorum: op.leaderStatus == nil || op.leaderStatus.resultTx == nil || op.leaderStatus.finalized")
		return false
	}
	mainHash := op.leaderStatus.signedResults[op.committee.OwnPeerIndex()].essenceHash
	sigShares := make([][]byte, 0, op.committee.Size())
	for i := range op.leaderStatus.signedResults {
		if op.leaderStatus.signedResults[i].essenceHash == mainHash {
			sigShares = append(sigShares, op.leaderStatus.signedResults[i].sigShare)
		}
	}
	if len(sigShares) < int(op.quorum()) {
		return false
	}
	// quorum detected
	err := op.aggregateSigShares(sigShares)
	if err != nil {
		op.log.Errorf("aggregateSigShares returned: %v", err)
		return false
	}
	if !op.leaderStatus.resultTx.SignaturesValid() {
		op.log.Errorf("something went wrong while finalizing result transaction: %v", err)
		return false
	}

	op.log.Infof("FINALIZED RESULT. Posting transaction to the Value Tangle. txid = %s",
		op.leaderStatus.resultTx.ID().String())

	nodeconn.PostTransactionToNodeAsyncWithRetry(op.leaderStatus.resultTx.Transaction, 2*time.Second, 7*time.Second, op.log)
	return true
}

// sets new state transaction and initializes respective variables
func (op *operator) setNewState(stateTx *sctransaction.Transaction, variableState state.VariableState) {
	op.stateTx = stateTx
	op.balances = nil
	op.getBalancesDeadline = time.Now()

	nextStateTransition := false
	if op.variableState != nil && variableState.StateIndex() == op.variableState.StateIndex()+1 {
		nextStateTransition = true
	}
	op.variableState = variableState

	op.resetLeader(stateTx.ID().Bytes())

	// computation requests and notifications about requests for the next state index
	// are brought to the current state next state list is cleared
	op.currentStateCompRequests, op.nextStateCompRequests =
		op.nextStateCompRequests, op.currentStateCompRequests
	op.nextStateCompRequests = op.nextStateCompRequests[:0]

	for _, req := range op.requests {
		if nextStateTransition {
			req.notificationsNextState, req.notificationsCurrentState = req.notificationsCurrentState, req.notificationsNextState
			setAllFalse(req.notificationsNextState)
			if req.reqMsg != nil {
				req.notificationsNextState[op.peerIndex()] = true
				req.notificationsCurrentState[op.peerIndex()] = true
			}
		} else {
			setAllFalse(req.notificationsNextState)
			setAllFalse(req.notificationsCurrentState)
		}
	}
}
