package genesis

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cespare/cp"
	"github.com/ethereum-optimism/optimism/op-bindings/bindings"
	"github.com/ethereum-optimism/optimism/op-chain-ops/deployer"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/tidwall/sjson"
)

const (
	// defaultDialTimeout is default duration the service will wait on
	// startup to make a connection to either the L1 or L2 backends.
	defaultDialTimeout = 5 * time.Second
	defaultL1URL       = "http://localhost"
	defaultRpcPort     = 8545
	defaultWsPort      = 8546
)

type FilePaths struct {
	outputRootPath string
	nodeDataPath   string
	nodekeyPath    string
	genesisPath    string
	kscnPath       string
	kscndPath      string
	kscndConfPath  string
	homiPath       string
}

func DefaultL1Url() string {
	return fmt.Sprintf("%s:%d", defaultL1URL, defaultRpcPort)
}

func DialEthClientWithTimeout(ctx context.Context, url string) (*ethclient.Client, error) {

	ctxt, cancel := context.WithTimeout(ctx, defaultDialTimeout)
	defer cancel()

	return ethclient.DialContext(ctxt, url)
}

func generateConfigurationsByHomi(homiPath string, chainID hexutil.Uint64, outputPath string) error {
	homiCmd := exec.Command(homiPath,
		"setup",
		"local",
		"--cn-num", "1",
		"--servicechain",
		"--serviceChainID", chainID.String(),
		"-o", outputPath,
	)
	if err := homiCmd.Run(); err != nil {
		return err
	}

	return nil
}

func modifyGeneratedGenesis(genesisPath string, chainID hexutil.Uint64, timestamp hexutil.Uint64) error {
	genesisBytes, err := ioutil.ReadFile(genesisPath)
	if err != nil {
		return err
	}

	genesisStr := string(genesisBytes)

	genesisStr, err = sjson.Set(genesisStr, "config.chainId", uint64(chainID))
	if err != nil {
		return err
	}

	genesisStr, err = sjson.Set(genesisStr, "config.deriveShaImpl", 0)
	if err != nil {
		return err
	}

	genesisStr, err = sjson.Set(genesisStr, "timestamp", timestamp.String())
	if err != nil {
		return err
	}

	for i := 0; i < 256; i++ {
		addr := common.BytesToAddress([]byte{byte(i)})
		genesisStr, err = sjson.Set(genesisStr, "alloc."+addr.String()+".balance", common.Big1.String())
		if err != nil {
			return err
		}
	}

	for _, devAccount := range DevAccounts {
		genesisStr, err = sjson.Set(genesisStr, "alloc."+devAccount.String()+".balance", devBalance.String())
		if err != nil {
			return err
		}
	}

	err = ioutil.WriteFile(genesisPath, []byte(genesisStr), os.FileMode(0644))
	if err != nil {
		return err
	}

	return nil
}

func initKlaytnL1(paths FilePaths, chainID hexutil.Uint64, rpcPort hexutil.Uint64, wsPort hexutil.Uint64) error {

	initCmd := exec.Command(paths.kscnPath,
		"init",
		"--datadir", paths.nodeDataPath,
		paths.genesisPath,
	)
	if err := initCmd.Run(); err != nil {
		return err
	}
	if err := cp.CopyFile(path.Join(paths.nodeDataPath, "klay/nodekey"), paths.nodekeyPath); err != nil {
		return err
	}

	confBytes, err := ioutil.ReadFile(paths.kscndConfPath)
	if err != nil {
		return err
	}

	nodeDataRelPath, err := filepath.Rel(filepath.Dir(paths.kscndPath), paths.nodeDataPath)
	if err != nil {
		return err
	}

	confString := string(confBytes)
	confString = strings.ReplaceAll(confString, "%DATA_DIR%", nodeDataRelPath)
	confString = strings.ReplaceAll(confString, "%CHAIN_ID%", strconv.FormatUint(uint64(chainID), 10))
	confString = strings.ReplaceAll(confString, "%RPC_PORT%", strconv.FormatUint(uint64(rpcPort), 10))
	confString = strings.ReplaceAll(confString, "%WS_PORT%", strconv.FormatUint(uint64(wsPort), 10))
	err = ioutil.WriteFile(paths.kscndConfPath, []byte(confString), os.FileMode(0644))
	if err != nil {
		return err
	}

	kscndStartCmd := exec.Command(paths.kscndPath, "start")
	kscndStartCmd.Dir = path.Dir(paths.kscndPath)
	if err := kscndStartCmd.Run(); err != nil {
		return err
	}

	return nil
}

func initScnDevnet(rpcPort hexutil.Uint64, wsPort hexutil.Uint64, chainID hexutil.Uint64, timestamp hexutil.Uint64, paths FilePaths) error {

	if err := generateConfigurationsByHomi(paths.homiPath, chainID, paths.outputRootPath); err != nil {
		return err
	}
	if err := modifyGeneratedGenesis(paths.genesisPath, chainID, timestamp); err != nil {
		return err
	}
	if err := initKlaytnL1(paths, chainID, rpcPort, wsPort); err != nil {
		return err
	}

	return nil
}

func deployL1ContractsAtKlaytn(config *DeployConfig, client *ethclient.Client) ([]deployer.Deployment, error) {
	deployer.ChainID = big.NewInt(int64(config.L1ChainID))

	constructors := make([]deployer.Constructor, 0)
	for _, proxy := range proxies {
		constructors = append(constructors, deployer.Constructor{
			Name: proxy,
		})
	}
	gasLimit := uint64(config.L2GenesisBlockGasLimit)
	if gasLimit == 0 {
		gasLimit = defaultL2GasLimit
	}
	constructors = append(constructors, []deployer.Constructor{
		{
			Name: "SystemConfig",
			Args: []interface{}{
				config.FinalSystemOwner,
				uint642Big(config.GasPriceOracleOverhead),
				uint642Big(config.GasPriceOracleScalar),
				config.BatchSenderAddress.Hash(), // left-padded 32 bytes value, version is zero anyway
				gasLimit,
				config.P2PSequencerAddress,
			},
		},
		{
			Name: "L2OutputOracle",
			Args: []interface{}{
				uint642Big(config.L2OutputOracleSubmissionInterval),
				uint642Big(config.L2BlockTime),
				big.NewInt(0),
				uint642Big(uint64(config.L1GenesisBlockTimestamp)),
				config.L2OutputOracleProposer,
				config.L2OutputOracleChallenger,
			},
		},
		{
			Name: "OptimismPortal",
			Args: []interface{}{
				uint642Big(config.FinalizationPeriodSeconds),
			},
		},
		{
			Name: "L1CrossDomainMessenger",
		},
		{
			Name: "L1StandardBridge",
		},
		{
			Name: "L1ERC721Bridge",
		},
		{
			Name: "OptimismMintableERC20Factory",
		},
		{
			Name: "AddressManager",
		},
		{
			Name: "ProxyAdmin",
			Args: []interface{}{
				common.Address{19: 0x01},
			},
		},
		{
			Name: "WETH9",
		},
	}...)
	return deployer.DeployWithClient(client, constructors, l1Deployer)
}

func upgradeProxies(config *DeployConfig, client *ethclient.Client, deployments []deployer.Deployment) error {
	depsByName := make(map[string]deployer.Deployment)
	depsByAddr := make(map[common.Address]deployer.Deployment)
	for _, dep := range deployments {
		depsByName[dep.Name] = dep
		depsByAddr[dep.Address] = dep
	}

	opts, err := bind.NewKeyedTransactorWithChainID(deployer.TestKey, deployer.ChainID)
	if err != nil {
		return err
	}
	sysCfgABI, err := bindings.SystemConfigMetaData.GetAbi()
	if err != nil {
		return err
	}
	gasLimit := uint64(config.L2GenesisBlockGasLimit)
	if gasLimit == 0 {
		gasLimit = defaultL2GasLimit
	}
	data, err := sysCfgABI.Pack(
		"initialize",
		config.FinalSystemOwner,
		uint642Big(config.GasPriceOracleOverhead),
		uint642Big(config.GasPriceOracleScalar),
		config.BatchSenderAddress.Hash(),
		gasLimit,
		config.P2PSequencerAddress,
	)
	if err != nil {
		return fmt.Errorf("cannot abi encode initialize for SystemConfig: %w", err)
	}
	if _, err := upgradeProxy(
		client,
		opts,
		depsByName["SystemConfigProxy"].Address,
		depsByName["SystemConfig"].Address,
		data,
	); err != nil {
		return err
	}

	l2ooABI, err := bindings.L2OutputOracleMetaData.GetAbi()
	if err != nil {
		return err
	}
	data, err = l2ooABI.Pack(
		"initialize",
		big.NewInt(0),
		uint642Big(uint64(config.L1GenesisBlockTimestamp)),
	)
	if err != nil {
		return fmt.Errorf("cannot abi encode initialize for L2OutputOracle: %w", err)
	}
	if _, err := upgradeProxy(
		client,
		opts,
		depsByName["L2OutputOracleProxy"].Address,
		depsByName["L2OutputOracle"].Address,
		data,
	); err != nil {
		return err
	}

	portalABI, err := bindings.OptimismPortalMetaData.GetAbi()
	if err != nil {
		return err
	}
	data, err = portalABI.Pack("initialize")
	if err != nil {
		return fmt.Errorf("cannot abi encode initialize for OptimismPortal: %w", err)
	}
	if _, err := upgradeProxy(
		client,
		opts,
		depsByName["OptimismPortalProxy"].Address,
		depsByName["OptimismPortal"].Address,
		data,
	); err != nil {
		return err
	}
	l1XDMABI, err := bindings.L1CrossDomainMessengerMetaData.GetAbi()
	if err != nil {
		return err
	}
	data, err = l1XDMABI.Pack(
		"initialize",
		config.FinalSystemOwner,
	)
	if err != nil {
		return fmt.Errorf("cannot abi encode initialize for L1CrossDomainMessenger: %w", err)
	}
	if _, err := upgradeProxy(
		client,
		opts,
		depsByName["L1CrossDomainMessengerProxy"].Address,
		depsByName["L1CrossDomainMessenger"].Address,
		data,
	); err != nil {
		return err
	}

	if _, err := upgradeProxy(
		client,
		opts,
		depsByName["L1StandardBridgeProxy"].Address,
		depsByName["L1StandardBridge"].Address,
		nil,
	); err != nil {
		return err
	}

	var lastUpgradeTx *types.Transaction
	if lastUpgradeTx, err = upgradeProxy(
		client,
		opts,
		depsByName["OptimismMintableERC20FactoryProxy"].Address,
		depsByName["OptimismMintableERC20Factory"].Address,
		nil,
	); err != nil {
		return err
	}

	if _, err := bind.WaitMined(context.Background(), client, lastUpgradeTx); err != nil {
		return err
	}

	return nil
}

func BuildKlaytnL1DevnetEnvironment(outputPath string, config *DeployConfig) (*types.Block, error) {
	ctx := context.Background()

	paths := FilePaths{
		outputRootPath: outputPath,
		nodeDataPath:   path.Join(outputPath, "nodedata"),
		nodekeyPath:    path.Join(outputPath, "keys/nodekey1"),
		genesisPath:    path.Join(outputPath, "scripts/genesis.json"),
		kscnPath:       path.Join(outputPath, "bin/kscn"),
		kscndPath:      path.Join(outputPath, "bin/kscnd"),
		kscndConfPath:  path.Join(outputPath, "conf/kscnd.conf"),
		homiPath:       path.Join(outputPath, "bin/homi"),
	}

	println("initialize klaytn service chain for devnet")
	if err := initScnDevnet(defaultRpcPort, defaultWsPort, hexutil.Uint64(config.L1ChainID), hexutil.Uint64(config.L1GenesisBlockTimestamp), paths); err != nil {
		return nil, err
	}
	time.Sleep(time.Second * 5)

	var l1Client *ethclient.Client
	var err error
	for i := 0; i < 5; i++ {
		l1RpcUrl := DefaultL1Url()
		l1Client, err = DialEthClientWithTimeout(ctx, l1RpcUrl)
		if err == nil {
			break
		}

		time.Sleep(time.Second * 5)
	}
	if err != nil {
		return nil, err
	}

	println("deploy l1 contracts")
	deployments, err := deployL1ContractsAtKlaytn(config, l1Client)
	if err != nil {
		return nil, err
	}

	println("upgrade l1 contract proxies")
	if err := upgradeProxies(config, l1Client, deployments); err != nil {
		return nil, err
	}

	println("get the l1 genesis block")
	l1StartBlock, err := l1Client.BlockByNumber(context.Background(), big.NewInt(0))
	if err != nil {
		return nil, err
	}

	kscndStopCmd := exec.Command(paths.kscndPath, "stop")
	kscndStopCmd.Dir = path.Dir(paths.kscndPath)
	if err := kscndStopCmd.Run(); err != nil {
		return nil, err
	}

	return l1StartBlock, nil
}
