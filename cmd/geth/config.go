// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// TODO: 환경 구성자 역할		사용자가 입력한 명령어/플래그/옵션들을 바탕으로 실제 작동할 node.Node 객체 생성

/*
	TODO: TOML 이란 ?
			설정파일을 위한 단순하고 명확한 문법의 구성 파일 포멧이다.
			cli 명령어 플래그 같은 것이 아닌 설정을 위해서 넣는 파일로 Geth --config 플래그를 통해서 TOML파일 설정을 읽는다.

			* CLI플래그나 TOML설정파일이나 최종적으로는 같은 구조체 getConfig로 파싱된다.
			  그리고 이 getCOnfig에 맞춰서 Node객체가 생성되는 것




	config.go

	default config -> toml -> flg등을 거쳐서 config 구조체를 만듦
	그 config 구조체를 바탕으로 makenode  노드까지 생성하는 기능을 담고 있다.
	근데 그 기능관련된 엔진은 node에 직점 정의되는 듯
*/

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/external"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/accounts/scwallet"
	"github.com/ethereum/go-ethereum/accounts/usbwallet"
	"github.com/ethereum/go-ethereum/beacon/blsync"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/naoina/toml"
	"github.com/urfave/cli/v2"
)

// cli command & flg 선언
var (
	dumpConfigCommand = &cli.Command{
		Action:      dumpConfig,
		Name:        "dumpconfig",
		Usage:       "Export configuration values in a TOML format",
		ArgsUsage:   "<dumpfile (optional)>",
		Flags:       slices.Concat(nodeFlags, rpcFlags),
		Description: `Export configuration values in TOML format (to stdout by default).`,
	}

	configFileFlag = &cli.StringFlag{
		Name:     "config",
		Usage:    "TOML configuration file",
		Category: flags.EthCategory,
	}
)

// These settings ensure that TOML keys use the same names as Go struct fields.
// toml.Config 구조체  	toml파일을 어떤방식으로 파싱할지 함수 선택
var tomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string {
		return key
	},
	FieldToKey: func(rt reflect.Type, field string) string {
		return field
	},
	MissingField: func(rt reflect.Type, field string) error {
		id := fmt.Sprintf("%s.%s", rt.String(), field)
		if deprecatedConfigFields[id] {
			log.Warn(fmt.Sprintf("Config field '%s' is deprecated and won't have any effect.", id))
			return nil
		}
		var link string
		if unicode.IsUpper(rune(rt.Name()[0])) && rt.PkgPath() != "main" {
			link = fmt.Sprintf(", see https://godoc.org/%s#%s for available fields", rt.PkgPath(), rt.Name())
		}
		return fmt.Errorf("field '%s' is not defined in %s%s", field, rt.String(), link)
	},
}

// 더이상 사용되지 않는 TOML 설정 항목들을 나열한 것			설정파일에 이런항목이 있다면 무시한다.
var deprecatedConfigFields = map[string]bool{
	"ethconfig.Config.EVMInterpreter":          true,
	"ethconfig.Config.EWASMInterpreter":        true,
	"ethconfig.Config.TrieCleanCacheJournal":   true,
	"ethconfig.Config.TrieCleanCacheRejournal": true,
	"ethconfig.Config.LightServ":               true,
	"ethconfig.Config.LightIngress":            true,
	"ethconfig.Config.LightEgress":             true,
	"ethconfig.Config.LightPeers":              true,
	"ethconfig.Config.LightNoPrune":            true,
	"ethconfig.Config.LightNoSyncServe":        true,
}

// EthStats 서버 url 관련 구조체
type ethstatsConfig struct {
	URL string `toml:",omitempty"`
}

// Geth의 전체 설정을 담는 통합 구조체
type gethConfig struct {
	Eth      ethconfig.Config
	Node     node.Config
	Ethstats ethstatsConfig
	Metrics  metrics.Config
}

// file 경로의 TOML파일을 읽고 gethConfig 구조체를 채우는 함수
func loadConfig(file string, cfg *gethConfig) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	err = tomlSettings.NewDecoder(bufio.NewReader(f)).Decode(cfg)
	// Add file name to errors that have a line number.
	if _, ok := err.(*toml.LineError); ok {
		err = errors.New(file + ", " + err.Error())
	}
	return err
}



// geth노드를 위한 default config 구조체를 리턴하는 함수
func defaultNodeConfig() node.Config {
	git, _ := version.VCS()
	cfg := node.DefaultConfig
	cfg.Name = clientIdentifier
	cfg.Version = version.WithCommit(git.Commit, git.Date)
	cfg.HTTPModules = append(cfg.HTTPModules, "eth")
	cfg.WSModules = append(cfg.WSModules, "eth")
	cfg.IPCPath = clientIdentifier + ".ipc"
	return cfg
}



// loadBaseConfig loads the gethConfig based on the given command line
// parameters and config file.

// default config + TOML file + CLI flg를 합쳐서 하나의 gethConfig파일을 만드는 함수
func loadBaseConfig(ctx *cli.Context) gethConfig {
	// Load defaults. 	1.기본적으로 일단 설정
	cfg := gethConfig{
		Eth:     ethconfig.Defaults,
		Node:    defaultNodeConfig(),
		Metrics: metrics.DefaultConfig,
	}

	// Load config file.  2.TOML file (==config file)을 덮어 씌움
	if file := ctx.String(configFileFlag.Name); file != "" {
		if err := loadConfig(file, &cfg); err != nil {
			utils.Fatalf("%v", err)
		}
	}

	// Apply flags.		3.cli flg로 명령인자받았으면 그걸로 또 덮어씌움
	utils.SetNodeConfig(ctx, &cfg.Node)
	return cfg
}



// makeConfigNode loads geth configuration and creates a blank node instance.
// TODO: Geth에서 노드를 생성하고 그 위에 account/eth/metric 설정을 적용하는 초기화 함수
func makeConfigNode(ctx *cli.Context) (*node.Node, gethConfig) {
	cfg := loadBaseConfig(ctx)			// config함수를 완성하고 cfg에 저장
	stack, err := node.New(&cfg.Node)	// cfg.node 설정에 맞게 geth 노드 객체를 생성
	if err != nil {
		utils.Fatalf("Failed to create the protocol stack: %v", err)
	}
	// Node doesn't by default populate account manager backends
	// 지갑등을 등록하는 과정
	if err := setAccountManagerBackends(stack.Config(), stack.AccountManager(), stack.KeyStoreDir()); err != nil {
		utils.Fatalf("Failed to set account manager backends: %v", err)
	}

	utils.SetEthConfig(ctx, stack, &cfg.Eth)	// cfg.eth 설정을 노드에 반영
	if ctx.IsSet(utils.EthStatsURLFlag.Name) {	// ethstats 플래그 있다면 그것도 반영
		cfg.Ethstats.URL = ctx.String(utils.EthStatsURLFlag.Name)
	}
	applyMetricConfig(ctx, &cfg)		// cfg.metric 설정을 반영

	return stack, cfg
}



// constructs the disclaimer text block which will be printed in the logs upon
// startup when Geth is running in dev mode.
// 개발자모드로 geth를 실행했을떄 로그로 출력되는 배너 문자열을 생성하는 함수
func constructDevModeBanner(ctx *cli.Context, cfg gethConfig) string {
	devModeBanner := `You are running Geth in --dev mode. Please note the following:

  1. This mode is only intended for fast, iterative development without assumptions on
     security or persistence.
  2. The database is created in memory unless specified otherwise. Therefore, shutting down
     your computer or losing power will wipe your entire block data and chain state for
     your dev environment.
  3. A random, pre-allocated developer account will be available and unlocked as
     eth.coinbase, which can be used for testing. The random dev account is temporary,
     stored on a ramdisk, and will be lost if your machine is restarted.
  4. Mining is enabled by default. However, the client will only seal blocks if transactions
     are pending in the mempool. The miner's minimum accepted gas price is 1.
  5. Networking is disabled; there is no listen-address, the maximum number of peers is set
     to 0, and discovery is disabled.
`
	if !ctx.IsSet(utils.DataDirFlag.Name) {
		devModeBanner += fmt.Sprintf(`

 Running in ephemeral mode.  The following account has been prefunded in the genesis:

       Account
       ------------------
       0x%x (10^49 ETH)
`, cfg.Eth.Miner.PendingFeeRecipient)
		if cfg.Eth.Miner.PendingFeeRecipient == utils.DeveloperAddr {
			devModeBanner += fmt.Sprintf(` 
       Private Key
       ------------------
       0x%x
`, crypto.FromECDSA(utils.DeveloperKey))
		}
	}

	return devModeBanner
}



// makeFullNode loads geth configuration and creates the Ethereum backend.
// TODO: makeFullNode	설정 config에 맞게 노드를 생성하고 여러추가적인 설정을한 뒤 그 노드를 리턴하는 함수
func makeFullNode(ctx *cli.Context) *node.Node {
	stack, cfg := makeConfigNode(ctx)			// cfg config로드 + 그 설정에 맞는 노드 객체 생성
	
	// 특정 테스트넷 osaka verkle 수동 설정
	if ctx.IsSet(utils.OverrideOsaka.Name) {
		v := ctx.Uint64(utils.OverrideOsaka.Name)
		cfg.Eth.OverrideOsaka = &v
	}
	if ctx.IsSet(utils.OverrideVerkle.Name) {
		v := ctx.Uint64(utils.OverrideVerkle.Name)
		cfg.Eth.OverrideVerkle = &v
	}

	// Start metrics export if enabled		메트릭 서버 설정
	utils.SetupMetrics(&cfg.Metrics)

	backend, eth := utils.RegisterEthService(stack, &cfg.Eth)	// ethereum 서비스 등록

	// Create gauge with geth system and build information
	// geth/info 메트릭 등록
	if eth != nil { // The 'eth' backend may be nil in light mode
		var protos []string
		for _, p := range eth.Protocols() {
			protos = append(protos, fmt.Sprintf("%v/%d", p.Name, p.Version))
		}
		metrics.NewRegisteredGaugeInfo("geth/info", nil).Update(metrics.GaugeInfoValue{
			"arch":      runtime.GOARCH,
			"os":        runtime.GOOS,
			"version":   cfg.Node.Version,
			"protocols": strings.Join(protos, ","),
		})
	}

	// Configure log filter RPC API.	RPC API의 필터 등록
	filterSystem := utils.RegisterFilterAPI(stack, backend, &cfg.Eth)

	// Configure GraphQL if requested.		GraphQL 기능 설정 optional
	if ctx.IsSet(utils.GraphQLEnabledFlag.Name) {
		utils.RegisterGraphQLService(stack, backend, filterSystem, &cfg.Node)
	}
	// Add the Ethereum Stats daemon if requested.	EthStats 서버 등록
	if cfg.Ethstats.URL != "" {
		utils.RegisterEthStatsService(stack, backend, cfg.Ethstats.URL)
	}
	// Configure synchronization override service   동기화 대상 강제 설정 (테스트용)
	var synctarget common.Hash
	if ctx.IsSet(utils.SyncTargetFlag.Name) {
		hex := hexutil.MustDecode(ctx.String(utils.SyncTargetFlag.Name))
		if len(hex) != common.HashLength {
			utils.Fatalf("invalid sync target length: have %d, want %d", len(hex), common.HashLength)
		}
		synctarget = common.BytesToHash(hex)
	}
	utils.RegisterSyncOverrideService(stack, eth, synctarget, ctx.Bool(utils.ExitWhenSyncedFlag.Name))

	// dev/beacon/catalyst mode 중 결정
	if ctx.IsSet(utils.DeveloperFlag.Name) {
		// Start dev mode.
		simBeacon, err := catalyst.NewSimulatedBeacon(ctx.Uint64(utils.DeveloperPeriodFlag.Name), cfg.Eth.Miner.PendingFeeRecipient, eth)
		if err != nil {
			utils.Fatalf("failed to register dev mode catalyst service: %v", err)
		}
		catalyst.RegisterSimulatedBeaconAPIs(stack, simBeacon)
		stack.RegisterLifecycle(simBeacon)

		banner := constructDevModeBanner(ctx, cfg)
		for _, line := range strings.Split(banner, "\n") {
			log.Warn(line)
		}
	} else if ctx.IsSet(utils.BeaconApiFlag.Name) {
		// Start blsync mode.
		srv := rpc.NewServer()
		srv.RegisterName("engine", catalyst.NewConsensusAPI(eth))
		blsyncer := blsync.NewClient(utils.MakeBeaconLightConfig(ctx))
		blsyncer.SetEngineRPC(rpc.DialInProc(srv))
		stack.RegisterLifecycle(blsyncer)
	} else {
		// Launch the engine API for interacting with external consensus client.
		err := catalyst.Register(stack, eth)
		if err != nil {
			utils.Fatalf("failed to register catalyst service: %v", err)
		}
	}
	return stack	// 마지막으로 생성 노드를 리턴함
}



// dumpConfig is the dumpconfig command.
// geth dumpconfig 커맨드 실행시 호출되는 함수로	현재의 설정 상태 gethConfig를 TOML로 출력 또는 저장하는 기능
func dumpConfig(ctx *cli.Context) error {
	_, cfg := makeConfigNode(ctx)
	comment := ""

	if cfg.Eth.Genesis != nil {
		cfg.Eth.Genesis = nil
		comment += "# Note: this config doesn't contain the genesis block.\n\n"
	}

	out, err := tomlSettings.Marshal(&cfg)
	if err != nil {
		return err
	}

	dump := os.Stdout
	if ctx.NArg() > 0 {
		dump, err = os.OpenFile(ctx.Args().Get(0), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		defer dump.Close()
	}
	dump.WriteString(comment)
	dump.Write(out)

	return nil
}




// geth실행시 사용자가 CLI 명령어로 입력한 플래그들을 gethConfig에 반영하는 역할을 한다.
func applyMetricConfig(ctx *cli.Context, cfg *gethConfig) {
	if ctx.IsSet(utils.MetricsEnabledFlag.Name) {
		cfg.Metrics.Enabled = ctx.Bool(utils.MetricsEnabledFlag.Name)
	}
	if ctx.IsSet(utils.MetricsEnabledExpensiveFlag.Name) {
		log.Warn("Expensive metrics are collected by default, please remove this flag", "flag", utils.MetricsEnabledExpensiveFlag.Name)
	}
	if ctx.IsSet(utils.MetricsHTTPFlag.Name) {
		cfg.Metrics.HTTP = ctx.String(utils.MetricsHTTPFlag.Name)
	}
	if ctx.IsSet(utils.MetricsPortFlag.Name) {
		cfg.Metrics.Port = ctx.Int(utils.MetricsPortFlag.Name)
	}
	if ctx.IsSet(utils.MetricsEnableInfluxDBFlag.Name) {
		cfg.Metrics.EnableInfluxDB = ctx.Bool(utils.MetricsEnableInfluxDBFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBEndpointFlag.Name) {
		cfg.Metrics.InfluxDBEndpoint = ctx.String(utils.MetricsInfluxDBEndpointFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBDatabaseFlag.Name) {
		cfg.Metrics.InfluxDBDatabase = ctx.String(utils.MetricsInfluxDBDatabaseFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBUsernameFlag.Name) {
		cfg.Metrics.InfluxDBUsername = ctx.String(utils.MetricsInfluxDBUsernameFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBPasswordFlag.Name) {
		cfg.Metrics.InfluxDBPassword = ctx.String(utils.MetricsInfluxDBPasswordFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBTagsFlag.Name) {
		cfg.Metrics.InfluxDBTags = ctx.String(utils.MetricsInfluxDBTagsFlag.Name)
	}
	if ctx.IsSet(utils.MetricsEnableInfluxDBV2Flag.Name) {
		cfg.Metrics.EnableInfluxDBV2 = ctx.Bool(utils.MetricsEnableInfluxDBV2Flag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBTokenFlag.Name) {
		cfg.Metrics.InfluxDBToken = ctx.String(utils.MetricsInfluxDBTokenFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBBucketFlag.Name) {
		cfg.Metrics.InfluxDBBucket = ctx.String(utils.MetricsInfluxDBBucketFlag.Name)
	}
	if ctx.IsSet(utils.MetricsInfluxDBOrganizationFlag.Name) {
		cfg.Metrics.InfluxDBOrganization = ctx.String(utils.MetricsInfluxDBOrganizationFlag.Name)
	}
	// Sanity-check the commandline flags. It is fine if some unused fields is part
	// of the toml-config, but we expect the commandline to only contain relevant
	// arguments, otherwise it indicates an error.
	var (
		enableExport   = ctx.Bool(utils.MetricsEnableInfluxDBFlag.Name)
		enableExportV2 = ctx.Bool(utils.MetricsEnableInfluxDBV2Flag.Name)
	)
	if enableExport || enableExportV2 {
		v1FlagIsSet := ctx.IsSet(utils.MetricsInfluxDBUsernameFlag.Name) ||
			ctx.IsSet(utils.MetricsInfluxDBPasswordFlag.Name)

		v2FlagIsSet := ctx.IsSet(utils.MetricsInfluxDBTokenFlag.Name) ||
			ctx.IsSet(utils.MetricsInfluxDBOrganizationFlag.Name) ||
			ctx.IsSet(utils.MetricsInfluxDBBucketFlag.Name)

		if enableExport && v2FlagIsSet {
			utils.Fatalf("Flags --influxdb.metrics.organization, --influxdb.metrics.token, --influxdb.metrics.bucket are only available for influxdb-v2")
		} else if enableExportV2 && v1FlagIsSet {
			utils.Fatalf("Flags --influxdb.metrics.username, --influxdb.metrics.password are only available for influxdb-v1")
		}
	}
}



// geth 노드가 실행될 떄 지갑관련 백엔드들을 등록하는 설정 함수
func setAccountManagerBackends(conf *node.Config, am *accounts.Manager, keydir string) error {
	scryptN := keystore.StandardScryptN
	scryptP := keystore.StandardScryptP
	if conf.UseLightweightKDF {
		scryptN = keystore.LightScryptN
		scryptP = keystore.LightScryptP
	}

	// Assemble the supported backends
	if len(conf.ExternalSigner) > 0 {
		log.Info("Using external signer", "url", conf.ExternalSigner)
		if extBackend, err := external.NewExternalBackend(conf.ExternalSigner); err == nil {
			am.AddBackend(extBackend)
			return nil
		} else {
			return fmt.Errorf("error connecting to external signer: %v", err)
		}
	}

	// For now, we're using EITHER external signer OR local signers.
	// If/when we implement some form of lockfile for USB and keystore wallets,
	// we can have both, but it's very confusing for the user to see the same
	// accounts in both externally and locally, plus very racey.
	am.AddBackend(keystore.NewKeyStore(keydir, scryptN, scryptP))
	if conf.USB {
		// Start a USB hub for Ledger hardware wallets
		if ledgerhub, err := usbwallet.NewLedgerHub(); err != nil {
			log.Warn(fmt.Sprintf("Failed to start Ledger hub, disabling: %v", err))
		} else {
			am.AddBackend(ledgerhub)
		}
		// Start a USB hub for Trezor hardware wallets (HID version)
		if trezorhub, err := usbwallet.NewTrezorHubWithHID(); err != nil {
			log.Warn(fmt.Sprintf("Failed to start HID Trezor hub, disabling: %v", err))
		} else {
			am.AddBackend(trezorhub)
		}
		// Start a USB hub for Trezor hardware wallets (WebUSB version)
		if trezorhub, err := usbwallet.NewTrezorHubWithWebUSB(); err != nil {
			log.Warn(fmt.Sprintf("Failed to start WebUSB Trezor hub, disabling: %v", err))
		} else {
			am.AddBackend(trezorhub)
		}
	}
	if len(conf.SmartCardDaemonPath) > 0 {
		// Start a smart card hub
		if schub, err := scwallet.NewHub(conf.SmartCardDaemonPath, scwallet.Scheme, keydir); err != nil {
			log.Warn(fmt.Sprintf("Failed to start smart card hub, disabling: %v", err))
		} else {
			am.AddBackend(schub)
		}
	}

	return nil
}
