package worker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/anyswap/CrossChain-Bridge/common"
	"github.com/anyswap/CrossChain-Bridge/mongodb"
	"github.com/anyswap/CrossChain-Bridge/params"
	"github.com/anyswap/CrossChain-Bridge/tokens"
)

var (
	swapChanSize       = 10
	swapinTaskChanMap  = make(map[string]chan *tokens.BuildTxArgs)
	swapoutTaskChanMap = make(map[string]chan *tokens.BuildTxArgs)

	errAlreadySwapped = errors.New("already swapped")
)

// StartSwapJob swap job
func StartSwapJob() {
	swapinNonces, swapoutNonces := mongodb.LoadAllSwapNonces()
	if nonceSetter, ok := tokens.DstBridge.(tokens.NonceSetter); ok {
		nonceSetter.InitNonces(swapinNonces)
	}
	if nonceSetter, ok := tokens.SrcBridge.(tokens.NonceSetter); ok {
		nonceSetter.InitNonces(swapoutNonces)
	}
	for _, pairCfg := range tokens.GetTokenPairsConfig() {
		AddSwapJob(pairCfg)
	}
}

// AddSwapJob add swap job
func AddSwapJob(pairCfg *tokens.TokenPairConfig) {
	pairID := strings.ToLower(pairCfg.PairID)
	swapinDcrmAddr := strings.ToLower(pairCfg.DestToken.DcrmAddress)
	if _, exist := swapinTaskChanMap[swapinDcrmAddr]; !exist {
		swapinTaskChanMap[swapinDcrmAddr] = make(chan *tokens.BuildTxArgs, swapChanSize)
		go processSwapTask(swapinTaskChanMap[swapinDcrmAddr])
	}
	swapoutDcrmAddr := strings.ToLower(pairCfg.SrcToken.DcrmAddress)
	if _, exist := swapoutTaskChanMap[swapoutDcrmAddr]; !exist {
		swapoutTaskChanMap[swapoutDcrmAddr] = make(chan *tokens.BuildTxArgs, swapChanSize)
		go processSwapTask(swapoutTaskChanMap[swapoutDcrmAddr])
	}

	go startSwapinSwapJob(pairID)
	go startSwapoutSwapJob(pairID)
}

func startSwapinSwapJob(pairID string) {
	logWorker("swap", "start swapin swap job")
	for {
		res, err := findSwapinsToSwap(pairID)
		if err != nil {
			logWorkerError("swapin", "find swapins error", err)
		}
		if len(res) > 0 {
			logWorker("swapin", "find swapins to swap", "count", len(res))
		}
		for _, swap := range res {
			err = processSwapinSwap(swap)
			switch err {
			case nil, errAlreadySwapped:
			default:
				logWorkerError("swapin", "process swapin swap error", err, "pairID", swap.PairID, "txid", swap.TxID, "bind", swap.Bind)
			}
		}
		restInJob(restIntervalInDoSwapJob)
	}
}

func startSwapoutSwapJob(pairID string) {
	logWorker("swapout", "start swapout swap job")
	for {
		res, err := findSwapoutsToSwap(pairID)
		if err != nil {
			logWorkerError("swapout", "find swapouts error", err)
		}
		if len(res) > 0 {
			logWorker("swapout", "find swapouts to swap", "count", len(res))
		}
		for _, swap := range res {
			err = processSwapoutSwap(swap)
			switch err {
			case nil, errAlreadySwapped:
			default:
				logWorkerError("swapout", "process swapout swap error", err, "pairID", swap.PairID, "txid", swap.TxID, "bind", swap.Bind)
			}
		}
		restInJob(restIntervalInDoSwapJob)
	}
}

func findSwapinsToSwap(pairID string) ([]*mongodb.MgoSwap, error) {
	status := mongodb.TxNotSwapped
	septime := getSepTimeInFind(maxDoSwapLifetime)
	return mongodb.FindSwapinsWithPairIDAndStatus(pairID, status, septime)
}

func findSwapoutsToSwap(pairID string) ([]*mongodb.MgoSwap, error) {
	status := mongodb.TxNotSwapped
	septime := getSepTimeInFind(maxDoSwapLifetime)
	return mongodb.FindSwapoutsWithPairIDAndStatus(pairID, status, septime)
}

func isSwapInBlacklist(swap *mongodb.MgoSwapResult) (isBlacked bool, err error) {
	isBlacked, err = mongodb.QueryBlacklist(swap.From, swap.PairID)
	if err != nil {
		return isBlacked, err
	}
	if !isBlacked && swap.Bind != swap.From {
		isBlacked, err = mongodb.QueryBlacklist(swap.Bind, swap.PairID)
		if err != nil {
			return isBlacked, err
		}
	}
	return isBlacked, nil
}

func processSwapinSwap(swap *mongodb.MgoSwap) (err error) {
	return processSwap(swap, true)
}

func processSwapoutSwap(swap *mongodb.MgoSwap) (err error) {
	return processSwap(swap, false)
}

func processSwap(swap *mongodb.MgoSwap, isSwapin bool) (err error) {
	pairID := swap.PairID
	txid := swap.TxID
	bind := swap.Bind

	res, err := mongodb.FindSwapResult(isSwapin, txid, pairID, bind)
	if err != nil {
		return err
	}

	err = preventDoubleSwap(res, isSwapin)
	if err != nil {
		return err
	}

	logWorker("swap", "start process swap", "pairID", pairID, "txid", txid, "bind", bind, "status", swap.Status, "isSwapin", isSwapin, "value", res.Value)

	fromTokenCfg, toTokenCfg := tokens.GetTokenConfigsByDirection(pairID, isSwapin)
	if fromTokenCfg == nil || toTokenCfg == nil {
		logWorkerTrace("swap", "swap is not configed", "pairID", pairID, "isSwapin", isSwapin)
		return nil
	}
	if fromTokenCfg.DisableSwap {
		logWorkerTrace("swap", "swap is disabled", "pairID", pairID, "isSwapin", isSwapin)
		return nil
	}
	isBlacked, err := isSwapInBlacklist(res)
	if err != nil {
		return err
	}
	if isBlacked {
		logWorkerTrace("swap", "address is in blacklist", "txid", txid, "bind", bind, "isSwapin", isSwapin)
		err = tokens.ErrAddressIsInBlacklist
		_ = mongodb.UpdateSwapStatus(isSwapin, txid, pairID, bind, mongodb.SwapInBlacklist, now(), err.Error())
		return nil
	}

	value, err := common.GetBigIntFromStr(res.Value)
	if err != nil {
		return fmt.Errorf("wrong value %v", res.Value)
	}

	swapType := getSwapType(isSwapin)
	args := &tokens.BuildTxArgs{
		SwapInfo: tokens.SwapInfo{
			Identifier: params.GetIdentifier(),
			PairID:     pairID,
			SwapID:     txid,
			SwapType:   swapType,
			TxType:     tokens.SwapTxType(swap.TxType),
			Bind:       bind,
		},
		From:        toTokenCfg.DcrmAddress,
		OriginValue: value,
	}

	return dispatchSwapTask(args)
}

func preventDoubleSwap(res *mongodb.MgoSwapResult, isSwapin bool) error {
	if res.SwapTx != "" || res.Status != mongodb.MatchTxEmpty || res.SwapHeight != 0 || len(res.OldSwapTxs) > 0 {
		_ = mongodb.UpdateSwapStatus(isSwapin, res.TxID, res.PairID, res.Bind, mongodb.TxProcessed, now(), "")
		return errAlreadySwapped
	}
	return nil
}

func getSwapType(isSwapin bool) tokens.SwapType {
	if isSwapin {
		return tokens.SwapinType
	}
	return tokens.SwapoutType
}

func dispatchSwapTask(args *tokens.BuildTxArgs) error {
	from := strings.ToLower(args.From)
	switch args.SwapType {
	case tokens.SwapinType:
		swapChan, exist := swapinTaskChanMap[from]
		if !exist {
			return fmt.Errorf("no swapin task channel for dcrm address '%v'", args.From)
		}
		swapChan <- args
	case tokens.SwapoutType:
		swapChan, exist := swapoutTaskChanMap[from]
		if !exist {
			return fmt.Errorf("no swapout task channel for dcrm address '%v'", args.From)
		}
		swapChan <- args
	default:
		return fmt.Errorf("wrong swap type '%v'", args.SwapType.String())
	}
	logWorker("doSwap", "dispatch swap task", "pairID", args.PairID, "txid", args.SwapID, "bind", args.Bind, "swapType", args.SwapType.String(), "value", args.OriginValue)
	return nil
}

func processSwapTask(swapChan <-chan *tokens.BuildTxArgs) {
	for {
		args := <-swapChan
		err := doSwap(args)
		switch err {
		case nil, errAlreadySwapped:
		default:
			logWorkerError("doSwap", "process failed", err, "pairID", args.PairID, "txid", args.SwapID, "swapType", args.SwapType.String(), "value", args.OriginValue)
		}
	}
}

func doSwap(args *tokens.BuildTxArgs) (err error) {
	pairID := args.PairID
	txid := args.SwapID
	bind := args.Bind
	swapType := args.SwapType
	originValue := args.OriginValue

	isSwapin := swapType == tokens.SwapinType
	resBridge := tokens.GetCrossChainBridge(!isSwapin)

	res, err := mongodb.FindSwapResult(isSwapin, txid, pairID, bind)
	if err != nil {
		return err
	}
	err = preventDoubleSwap(res, isSwapin)
	if err != nil {
		return err
	}

	logWorker("doSwap", "start to process", "pairID", pairID, "txid", txid, "bind", bind, "isSwapin", isSwapin, "value", originValue)

	rawTx, err := resBridge.BuildRawTransaction(args)
	if err != nil {
		logWorkerError("doSwap", "build tx failed", err, "txid", txid, "bind", bind, "isSwapin", isSwapin)
		return err
	}

	swapNonce := args.GetTxNonce()

	var signedTx interface{}
	var txHash string
	tokenCfg := resBridge.GetTokenConfig(pairID)
	if tokenCfg.GetDcrmAddressPrivateKey() != nil {
		signedTx, txHash, err = resBridge.SignTransaction(rawTx, pairID)
	} else {
		signedTx, txHash, err = resBridge.DcrmSignTransaction(rawTx, args.GetExtraArgs())
	}
	if err != nil {
		logWorkerError("doSwap", "sign tx failed", err, "txid", txid, "bind", bind, "isSwapin", isSwapin)
		return err
	}

	// update database before sending transaction
	matchTx := &MatchTx{
		SwapTx:    txHash,
		SwapValue: tokens.CalcSwappedValue(pairID, originValue, isSwapin).String(),
		SwapType:  swapType,
		SwapNonce: swapNonce,
	}
	err = updateSwapResult(txid, pairID, bind, matchTx)
	if err != nil {
		logWorkerError("doSwap", "update swap result failed", err, "txid", txid, "bind", bind, "isSwapin", isSwapin)
		return err
	}

	err = mongodb.UpdateSwapStatus(isSwapin, txid, pairID, bind, mongodb.TxProcessed, now(), "")
	if err != nil {
		logWorkerError("doSwap", "update swap status failed", err, "txid", txid, "bind", bind, "isSwapin", isSwapin)
		return err
	}

	err = sendSignedTransaction(resBridge, signedTx, txid, pairID, bind, isSwapin)
	if err == nil {
		if nonceSetter, ok := resBridge.(tokens.NonceSetter); ok {
			nonceSetter.SetNonce(pairID, swapNonce+1) // increase for next usage
		}
	}
	return err
}
