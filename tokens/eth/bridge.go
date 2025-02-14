package eth

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/anyswap/CrossChain-Bridge/common"
	"github.com/anyswap/CrossChain-Bridge/log"
	"github.com/anyswap/CrossChain-Bridge/tokens"
	"github.com/anyswap/CrossChain-Bridge/types"
)

const (
	netMainnet = "mainnet"
	netRinkeby = "rinkeby"
	netCustom  = "custom"
)

// Bridge eth bridge
type Bridge struct {
	*tokens.CrossChainBridgeBase
	*NonceSetterBase
	Signer        types.Signer
	SignerChainID *big.Int
}

// NewCrossChainBridge new bridge
func NewCrossChainBridge(isSrc bool) *Bridge {
	return &Bridge{
		CrossChainBridgeBase: tokens.NewCrossChainBridgeBase(isSrc),
		NonceSetterBase:      NewNonceSetterBase(),
	}
}

// SetChainAndGateway set chain and gateway config
func (b *Bridge) SetChainAndGateway(chainCfg *tokens.ChainConfig, gatewayCfg *tokens.GatewayConfig) {
	b.CrossChainBridgeBase.SetChainAndGateway(chainCfg, gatewayCfg)
	b.VerifyChainID()
	b.Init()
}

// Init init after verify
func (b *Bridge) Init() {
	InitExtCodeParts()
	b.InitLatestBlockNumber()
}

// VerifyChainID verify chain id
func (b *Bridge) VerifyChainID() {
	networkID := strings.ToLower(b.ChainConfig.NetID)
	switch networkID {
	case netMainnet, netRinkeby:
	case netCustom:
	default:
		log.Fatalf("unsupported ethereum network: %v", b.ChainConfig.NetID)
	}

	var (
		chainID *big.Int
		err     error
	)

	for i := 0; i < 5; i++ {
		chainID, err = b.GetSignerChainID()
		if err == nil {
			break
		}
		log.Errorf("can not get gateway chainID. %v", err)
		log.Println("retry query gateway", b.GatewayConfig.APIAddress)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		log.Fatal("get chain ID failed", "err", err)
	}

	panicMismatchChainID := func() {
		log.Fatalf("gateway chainID %v is not %v", chainID, b.ChainConfig.NetID)
	}

	switch networkID {
	case netMainnet:
		if chainID.Uint64() != 1 {
			panicMismatchChainID()
		}
	case netRinkeby:
		if chainID.Uint64() != 4 {
			panicMismatchChainID()
		}
	case netCustom:
	default:
		log.Fatalf("unsupported ethereum network %v", networkID)
	}

	b.SignerChainID = chainID
	b.Signer = types.MakeSigner("EIP155", chainID)

	log.Info("VerifyChainID succeed", "networkID", networkID, "chainID", chainID)
}

// VerifyTokenConfig verify token config
func (b *Bridge) VerifyTokenConfig(tokenCfg *tokens.TokenConfig) (err error) {
	if !b.IsValidAddress(tokenCfg.DcrmAddress) {
		return fmt.Errorf("invalid dcrm address: %v", tokenCfg.DcrmAddress)
	}
	if b.IsSrc && !b.IsValidAddress(tokenCfg.DepositAddress) {
		return fmt.Errorf("invalid deposit address: %v", tokenCfg.DepositAddress)
	}
	if tokenCfg.IsDelegateContract {
		return b.verifyDelegateContract(tokenCfg)
	}

	err = b.verifyDecimals(tokenCfg)
	if err != nil {
		return err
	}

	err = b.verifyContractAddress(tokenCfg)
	if err != nil {
		return err
	}

	return nil
}

func (b *Bridge) verifyDelegateContract(tokenCfg *tokens.TokenConfig) error {
	if tokenCfg.DelegateToken == "" {
		return nil
	}
	// keccak256 'proxyToken()' is '0x4faaefae'
	res, err := b.CallContract(tokenCfg.ContractAddress, common.FromHex("0x4faaefae"), "latest")
	if err != nil {
		return err
	}
	proxyToken := common.HexToAddress(res)
	if common.HexToAddress(tokenCfg.DelegateToken) != proxyToken {
		return fmt.Errorf("mismatch 'DelegateToken', has %v, want %v", tokenCfg.DelegateToken, proxyToken.String())
	}
	return nil
}

func (b *Bridge) verifyDecimals(tokenCfg *tokens.TokenConfig) error {
	configedDecimals := *tokenCfg.Decimals
	switch strings.ToUpper(tokenCfg.Symbol) {
	case "ETH", "FSN":
		if configedDecimals != 18 {
			return fmt.Errorf("invalid decimals: want 18 but have %v", configedDecimals)
		}
		log.Info(tokenCfg.Symbol+" verify decimals success", "decimals", configedDecimals)
	}

	if tokenCfg.IsErc20() {
		decimals, err := b.GetErc20Decimals(tokenCfg.ContractAddress)
		if err != nil {
			log.Error("get erc20 decimals failed", "err", err)
			return err
		}
		if decimals != configedDecimals {
			return fmt.Errorf("invalid decimals for %v, want %v but configed %v", tokenCfg.Symbol, decimals, configedDecimals)
		}
		log.Info(tokenCfg.Symbol+" verify decimals success", "decimals", configedDecimals)
	}
	return nil
}

func (b *Bridge) verifyContractAddress(tokenCfg *tokens.TokenConfig) error {
	if tokenCfg.ContractAddress != "" {
		if !b.IsValidAddress(tokenCfg.ContractAddress) {
			return fmt.Errorf("invalid contract address: %v", tokenCfg.ContractAddress)
		}
		switch {
		case !b.IsSrc:
			if err := b.VerifyMbtcContractAddress(tokenCfg.ContractAddress); err != nil {
				return fmt.Errorf("wrong contract address: %v, %v", tokenCfg.ContractAddress, err)
			}
		case tokenCfg.IsErc20():
			if err := b.VerifyErc20ContractAddress(tokenCfg.ContractAddress, tokenCfg.ContractCodeHash, tokenCfg.IsProxyErc20()); err != nil {
				return fmt.Errorf("wrong contract address: %v, %v", tokenCfg.ContractAddress, err)
			}
		default:
			return fmt.Errorf("unsupported type of contract address '%v' in source chain, please assign SrcToken.ID (eg. ERC20) in config file", tokenCfg.ContractAddress)
		}
		log.Info("verify contract address pass", "address", tokenCfg.ContractAddress)
	}
	return nil
}

// InitLatestBlockNumber init latest block number
func (b *Bridge) InitLatestBlockNumber() {
	var (
		latest uint64
		err    error
	)

	for {
		latest, err = b.GetLatestBlockNumber()
		if err == nil {
			tokens.SetLatestBlockHeight(latest, b.IsSrc)
			log.Info("get latst block number succeed.", "number", latest, "BlockChain", b.ChainConfig.BlockChain, "NetID", b.ChainConfig.NetID)
			break
		}
		log.Error("get latst block number failed.", "BlockChain", b.ChainConfig.BlockChain, "NetID", b.ChainConfig.NetID, "err", err)
		log.Println("retry query gateway", b.GatewayConfig.APIAddress)
		time.Sleep(3 * time.Second)
	}
}
