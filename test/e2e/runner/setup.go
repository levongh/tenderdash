// nolint: gosec
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/crypto/bls12381"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/privval"
	e2e "github.com/tendermint/tendermint/test/e2e/pkg"
	"github.com/tendermint/tendermint/types"
)

const (
	AppAddressTCP  = "tcp://127.0.0.1:30000"
	AppAddressUNIX = "unix:///var/run/app.sock"

	PrivvalAddressTCP      = "tcp://0.0.0.0:27559"
	PrivvalAddressUNIX     = "unix:///var/run/privval.sock"
	PrivvalAddressDashCore = "127.0.0.1:19998"
	PrivvalKeyFile         = "config/priv_validator_key.json"
	PrivvalStateFile       = "data/priv_validator_state.json"
	PrivvalDummyKeyFile    = "config/dummy_validator_key.json"
	PrivvalDummyStateFile  = "data/dummy_validator_state.json"
)

// Setup sets up the testnet configuration.
func Setup(testnet *e2e.Testnet) error {
	logger.Info(fmt.Sprintf("Generating testnet files in %q", testnet.Dir))

	err := os.MkdirAll(testnet.Dir, os.ModePerm)
	if err != nil {
		return err
	}

	compose, err := MakeDockerCompose(testnet)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(testnet.Dir, "docker-compose.yml"), compose, 0644)
	if err != nil {
		return err
	}

	genesisNodes, err := initGenesisForEveryNode(testnet)
	if err != nil {
		return err
	}
	withModificators(genesisNodes, pubkeyResettingModificator(shouldResetPubkeys()))

	for _, node := range testnet.Nodes {
		genesis, ok := genesisNodes[node.Mode]
		if !ok {
			return fmt.Errorf("node has unsupported node type: %s", node.Mode)
		}

		nodeDir := filepath.Join(testnet.Dir, node.Name)

		dirs := []string{
			filepath.Join(nodeDir, "config"),
			filepath.Join(nodeDir, "data"),
			filepath.Join(nodeDir, "data", "app"),
		}
		for _, dir := range dirs {
			// light clients don't need an app directory
			if node.Mode == e2e.ModeLight && strings.Contains(dir, "app") {
				continue
			}
			err := os.MkdirAll(dir, 0755)
			if err != nil {
				return err
			}
		}

		cfg, err := MakeConfig(node)
		if err != nil {
			return err
		}
		config.WriteConfigFile(filepath.Join(nodeDir, "config", "config.toml"), cfg) // panics

		appCfg, err := MakeAppConfig(node)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(filepath.Join(nodeDir, "config", "app.toml"), appCfg, 0644)
		if err != nil {
			return err
		}

		if node.Mode == e2e.ModeLight {
			// stop early if a light client
			// Set up a dummy validator for light client verification.
			pv, err := newDefaultFilePV(node, nodeDir)
			if err != nil {
				return err
			}
			pv.Save()
			continue
		}

		err = genesis.SaveAs(filepath.Join(nodeDir, "config", "genesis.json"))
		if err != nil {
			return err
		}

		err = (&p2p.NodeKey{PrivKey: node.NodeKey}).SaveAs(filepath.Join(nodeDir, "config", "node_key.json"))
		if err != nil {
			return err
		}

		if node.Mode == e2e.ModeValidator {
			pv, err := newFilePVFromNode(node, nodeDir)
			if err != nil {
				return err
			}
			pv.Save()
		}
		// Set up a dummy validator. Tenderdash requires a file PV even when not used, so we
		// give it a dummy such that it will fail if it actually tries to use it.
		pv, err := newDefaultFilePV(node, nodeDir)
		if err != nil {
			return err
		}
		pv.Save()
	}

	return nil
}

// MakeDockerCompose generates a Docker Compose config for a testnet.
func MakeDockerCompose(testnet *e2e.Testnet) ([]byte, error) {
	// Must use version 2 Docker Compose format, to support IPv6.
	tmpl, err := template.New("docker-compose").Funcs(template.FuncMap{
		"misbehaviorsToString": func(misbehaviors map[int64]string) string {
			str := ""
			for height, misbehavior := range misbehaviors {
				// after the first behavior set, a comma must be prepended
				if str != "" {
					str += ","
				}
				heightString := strconv.Itoa(int(height))
				str += misbehavior + "," + heightString
			}
			return str
		},
		"debugPort": func(index int) int {
			return 40000 + index + 1
		},
	}).Parse(`version: '2.4'

networks:
  {{ .Name }}:
    labels:
      e2e: true
    driver: bridge
{{- if .IPv6 }}
    enable_ipv6: true
{{- end }}
    ipam:
      driver: default
      config:
      - subnet: {{ .IP }}

services:
{{- range $index, $node :=  .Nodes }}
  {{ .Name }}:
    labels:
      e2e: true
    container_name: {{ .Name }}
    image: tenderdash/e2e-node
{{- if eq .ABCIProtocol "builtin" }}
    entrypoint: /usr/bin/entrypoint-builtin
{{- else if .Misbehaviors }}
    entrypoint: /usr/bin/entrypoint-maverick
    command: ["node", "--misbehaviors", "{{ misbehaviorsToString .Misbehaviors }}"]
{{- end }}
    init: true
{{- if $.Debug }}
    environment:
    - DEBUG=1
{{- end }}
    ports:
    - 26656
    - {{ if .ProxyPort }}{{ .ProxyPort }}:{{ end }}26657
    - 6060
{{- if $.Debug }}
    - {{ debugPort $index }}:40000
{{- end }}
    volumes:
    - ./{{ .Name }}:/tenderdash
{{- if ne $.PreCompiledAppPath "" }}
    - {{ $.PreCompiledAppPath }}:/usr/bin/app
{{- end }}
    networks:
      {{ $.Name }}:
        ipv{{ if $.IPv6 }}6{{ else }}4{{ end}}_address: {{ .IP }}

{{end}}`)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	data := &struct {
		*e2e.Testnet
		PreCompiledAppPath string
		Debug              bool
	}{
		Testnet:            testnet,
		PreCompiledAppPath: os.Getenv("PRE_COMPILED_APP_PATH"),
		Debug:              os.Getenv("DEBUG") != "",
	}
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MakeGenesis generates a genesis document.
func MakeGenesis(testnet *e2e.Testnet, genesisTime time.Time) (types.GenesisDoc, error) {
	genesis := types.GenesisDoc{
		GenesisTime:                  genesisTime,
		ChainID:                      testnet.Name,
		ConsensusParams:              types.DefaultConsensusParams(),
		InitialHeight:                testnet.InitialHeight,
		InitialCoreChainLockedHeight: testnet.InitialCoreHeight,
		ThresholdPublicKey:           testnet.ThresholdPublicKey,
		QuorumType:                   testnet.QuorumType,
		QuorumHash:                   testnet.QuorumHash,
	}
	for validator, pubkey := range testnet.Validators {
		genesis.Validators = append(genesis.Validators, types.GenesisValidator{
			Name:      validator.Name,
			PubKey:    pubkey,
			ProTxHash: validator.ProTxHash,
			Power:     types.DefaultDashVotingPower,
		})
	}
	// The validator set will be sorted internally by Tenderdash ranked by power,
	// but we sort it here as well so that all genesis files are identical.
	sort.Slice(genesis.Validators, func(i, j int) bool {
		return strings.Compare(genesis.Validators[i].Name, genesis.Validators[j].Name) == -1
	})
	if len(testnet.InitialState) > 0 {
		appState, err := json.Marshal(testnet.InitialState)
		if err != nil {
			return genesis, err
		}
		genesis.AppState = appState
	}
	return genesis, genesis.ValidateAndComplete()
}

// MakeConfig generates a Tenderdash config for a node.
func MakeConfig(node *e2e.Node) (*config.Config, error) {
	cfg := config.DefaultConfig()
	cfg.Moniker = node.Name
	cfg.ProxyApp = AppAddressTCP
	cfg.RPC.ListenAddress = "tcp://0.0.0.0:26657"
	cfg.RPC.PprofListenAddress = ":6060"
	cfg.P2P.ExternalAddress = fmt.Sprintf("tcp://%v", node.AddressP2P(false))
	cfg.P2P.AddrBookStrict = false
	cfg.DBBackend = node.Database
	cfg.StateSync.DiscoveryTime = 5 * time.Second
	cfg.Consensus.AppHashSize = crypto.DefaultHashSize
	cfg.BaseConfig.LogLevel = "debug"

	switch node.ABCIProtocol {
	case e2e.ProtocolUNIX:
		cfg.ProxyApp = AppAddressUNIX
	case e2e.ProtocolTCP:
		cfg.ProxyApp = AppAddressTCP
	case e2e.ProtocolGRPC:
		cfg.ProxyApp = AppAddressTCP
		cfg.ABCI = "grpc"
	case e2e.ProtocolBuiltin:
		cfg.ProxyApp = ""
		cfg.ABCI = ""
	default:
		return nil, fmt.Errorf("unexpected ABCI protocol setting %q", node.ABCIProtocol)
	}

	// Tenderdash errors if it does not have a privval key set up, regardless of whether
	// it's actually needed (e.g. for remote KMS or non-validators). We set up a dummy
	// key here by default, and use the real key for actual validators that should use
	// the file privval.
	cfg.PrivValidatorListenAddr = ""
	cfg.PrivValidatorKey = PrivvalDummyKeyFile
	cfg.PrivValidatorState = PrivvalDummyStateFile

	if node.PrivvalProtocol == e2e.ProtocolDashCore {
		cfg.PrivValidatorCoreRPCHost = "127.0.0.1:19998"
	}

	switch node.Mode {
	case e2e.ModeValidator:
		switch node.PrivvalProtocol {
		case e2e.ProtocolFile:
			cfg.PrivValidatorKey = PrivvalKeyFile
			cfg.PrivValidatorState = PrivvalStateFile
		case e2e.ProtocolUNIX:
			cfg.PrivValidatorListenAddr = PrivvalAddressUNIX
		case e2e.ProtocolTCP:
			cfg.PrivValidatorListenAddr = PrivvalAddressTCP
		case e2e.ProtocolDashCore:
			cfg.PrivValidatorKey = PrivvalKeyFile
			cfg.PrivValidatorState = PrivvalStateFile
		default:
			return nil, fmt.Errorf("invalid privval protocol setting %q", node.PrivvalProtocol)
		}
	case e2e.ModeSeed:
		cfg.P2P.SeedMode = true
		cfg.P2P.PexReactor = true
	case e2e.ModeFull, e2e.ModeLight:
		// Don't need to do anything, since we're using a dummy privval key by default.
	default:
		return nil, fmt.Errorf("unexpected mode %q", node.Mode)
	}

	if node.FastSync == "" {
		cfg.FastSyncMode = false
	} else {
		cfg.FastSync.Version = node.FastSync
	}

	if node.StateSync {
		cfg.StateSync.Enable = true
		cfg.StateSync.RPCServers = []string{}
		for _, peer := range node.Testnet.ArchiveNodes() {
			if peer.Name == node.Name {
				continue
			}
			cfg.StateSync.RPCServers = append(cfg.StateSync.RPCServers, peer.AddressRPC())
		}
		if len(cfg.StateSync.RPCServers) < 2 {
			return nil, errors.New("unable to find 2 suitable state sync RPC servers")
		}
	}

	cfg.P2P.Seeds = ""
	for _, seed := range node.Seeds {
		if len(cfg.P2P.Seeds) > 0 {
			cfg.P2P.Seeds += ","
		}
		cfg.P2P.Seeds += seed.AddressP2P(true)
	}
	cfg.P2P.PersistentPeers = ""
	for _, peer := range node.PersistentPeers {
		if len(cfg.P2P.PersistentPeers) > 0 {
			cfg.P2P.PersistentPeers += ","
		}
		cfg.P2P.PersistentPeers += peer.AddressP2P(true)
	}
	return cfg, nil
}

// MakeAppConfig generates an ABCI application config for a node.
func MakeAppConfig(node *e2e.Node) ([]byte, error) {
	cfg := map[string]interface{}{
		"chain_id":            node.Testnet.Name,
		"dir":                 "data/app",
		"listen":              AppAddressUNIX,
		"mode":                node.Mode,
		"proxy_port":          node.ProxyPort,
		"privval_server_type": "dashcore",
		"privval_server":      PrivvalAddressDashCore,
		"protocol":            "socket",
		"persist_interval":    node.PersistInterval,
		"snapshot_interval":   node.SnapshotInterval,
		"retain_blocks":       node.RetainBlocks,
		"key_type":            bls12381.KeyType,
	}
	switch node.ABCIProtocol {
	case e2e.ProtocolUNIX:
		cfg["listen"] = AppAddressUNIX
	case e2e.ProtocolTCP:
		cfg["listen"] = AppAddressTCP
	case e2e.ProtocolGRPC:
		cfg["listen"] = AppAddressTCP
		cfg["protocol"] = "grpc"
	case e2e.ProtocolBuiltin:
		delete(cfg, "listen")
		cfg["protocol"] = "builtin"
	default:
		return nil, fmt.Errorf("unexpected ABCI protocol setting %q", node.ABCIProtocol)
	}
	if node.Mode == e2e.ModeValidator {
		switch node.PrivvalProtocol {
		case e2e.ProtocolFile:
		case e2e.ProtocolDashCore:
			cfg["privval_server_type"] = "dashcore"
			cfg["privval_server"] = PrivvalAddressDashCore
		case e2e.ProtocolTCP:
			cfg["privval_server_type"] = "tcp"
			cfg["privval_server"] = PrivvalAddressTCP
			cfg["privval_key"] = PrivvalKeyFile
			cfg["privval_state"] = PrivvalStateFile
		case e2e.ProtocolUNIX:
			cfg["privval_server_type"] = "unix"
			cfg["privval_server"] = PrivvalAddressUNIX
			cfg["privval_key"] = PrivvalKeyFile
			cfg["privval_state"] = PrivvalStateFile
		default:
			return nil, fmt.Errorf("unexpected privval protocol setting %q", node.PrivvalProtocol)
		}
	} else if node.PrivvalProtocol == e2e.ProtocolDashCore {
		cfg["privval_server_type"] = "dashcore"
		cfg["privval_server"] = PrivvalAddressDashCore
	}

	misbehaviors := make(map[string]string)
	for height, misbehavior := range node.Misbehaviors {
		misbehaviors[strconv.Itoa(int(height))] = misbehavior
	}
	cfg["misbehaviors"] = misbehaviors

	if len(node.Testnet.ValidatorUpdates) > 0 {
		validatorUpdates := map[string]map[string]string{}
		for height, validators := range node.Testnet.ValidatorUpdates {
			updateVals := map[string]string{}
			for node, pubkey := range validators {
				updateVals[hex.EncodeToString(node.ProTxHash.Bytes())] = base64.StdEncoding.EncodeToString(pubkey.Bytes())
			}
			validatorUpdates[fmt.Sprintf("%v", height)] = updateVals
		}
		cfg["validator_update"] = validatorUpdates

		thresholdPublicKeyUpdates := map[string]string{}
		for height, thresholdPublicKey := range node.Testnet.ThresholdPublicKeyUpdates {

			thresholdPublicKeyUpdates[fmt.Sprintf("%v", height)] = base64.StdEncoding.EncodeToString(thresholdPublicKey.Bytes())
		}
		cfg["threshold_public_key_update"] = thresholdPublicKeyUpdates

		quorumHashUpdates := map[string]string{}
		for height, quorumHash := range node.Testnet.QuorumHashUpdates {

			quorumHashUpdates[fmt.Sprintf("%v", height)] = hex.EncodeToString(quorumHash.Bytes())
		}
		cfg["quorum_hash_update"] = quorumHashUpdates
	}
	if len(node.Testnet.ChainLockUpdates) > 0 {
		chainLockUpdates := map[string]string{}
		for height, coreHeight := range node.Testnet.ChainLockUpdates {
			chainLockUpdates[fmt.Sprintf("%v", height)] = fmt.Sprintf("%v", coreHeight)
		}
		cfg["chainlock_updates"] = chainLockUpdates
	}

	var buf bytes.Buffer
	err := toml.NewEncoder(&buf).Encode(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to generate app config: %w", err)
	}
	return buf.Bytes(), nil
}

// UpdateConfigStateSync updates the state sync config for a node.
func UpdateConfigStateSync(node *e2e.Node, height int64, hash []byte) error {
	cfgPath := filepath.Join(node.Testnet.Dir, node.Name, "config", "config.toml")

	// FIXME Apparently there's no function to simply load a config file without
	// involving the entire Viper apparatus, so we'll just resort to regexps.
	bz, err := ioutil.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	bz = regexp.MustCompile(`(?m)^trust_height =.*`).ReplaceAll(bz, []byte(fmt.Sprintf(`trust_height = %v`, height)))
	bz = regexp.MustCompile(`(?m)^trust_hash =.*`).ReplaceAll(bz, []byte(fmt.Sprintf(`trust_hash = "%X"`, hash)))
	return ioutil.WriteFile(cfgPath, bz, 0644)
}

func newDefaultFilePV(node *e2e.Node, nodeDir string) (*privval.FilePV, error) {
	return privval.NewFilePVWithOptions(
		privval.WithPrivateKeysMap(node.PrivvalKeys),
		privval.WithProTxHash(crypto.RandProTxHash()),
		privval.WithUpdateHeights(node.PrivvalUpdateHeights),
		privval.WithKeyAndStateFilePaths(
			filepath.Join(nodeDir, PrivvalDummyKeyFile),
			filepath.Join(nodeDir, PrivvalDummyStateFile),
		),
	)
}

func newFilePVFromNode(node *e2e.Node, nodeDir string) (*privval.FilePV, error) {
	return privval.NewFilePVWithOptions(
		privval.WithPrivateKeysMap(node.PrivvalKeys),
		privval.WithProTxHash(node.ProTxHash),
		privval.WithUpdateHeights(node.PrivvalUpdateHeights),
		privval.WithKeyAndStateFilePaths(
			filepath.Join(nodeDir, PrivvalKeyFile),
			filepath.Join(nodeDir, PrivvalStateFile),
		),
	)
}

func pubkeyResettingModificator(isValidator bool) func(genesis map[e2e.Mode]types.GenesisDoc) {
	return func(genesis map[e2e.Mode]types.GenesisDoc) {
		if isValidator {
			resetPubkey(genesis[e2e.ModeFull].Validators)
		}
	}
}

func resetPubkey(vals []types.GenesisValidator) {
	for i := 0; i < len(vals); i++ {
		vals[i].PubKey = nil
	}
}

func shouldResetPubkeys() bool {
	val, ok := os.LookupEnv("FULLNODE_PUBKEY_RESET")
	if !ok {
		return false
	}
	return val == "true" || val == "1"
}

func initGenesisForEveryNode(testnet *e2e.Testnet) (map[e2e.Mode]types.GenesisDoc, error) {
	genesis := make(map[e2e.Mode]types.GenesisDoc)
	genesisTime := time.Now()
	for _, tn := range testnet.Nodes {
		if _, ok := genesis[tn.Mode]; ok {
			continue
		}
		genDoc, err := MakeGenesis(testnet, genesisTime)
		if err != nil {
			return nil, err
		}
		genesis[tn.Mode] = genDoc
	}
	return genesis, nil
}

func withModificators(genesis map[e2e.Mode]types.GenesisDoc, modFuns ...func(map[e2e.Mode]types.GenesisDoc)) {
	for _, fn := range modFuns {
		fn(genesis)
	}
}
