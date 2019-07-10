package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path"
	"runtime"
	"time"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/harmony-one/bls/ffi/go/bls"

	"github.com/harmony-one/harmony/accounts/keystore"
	"github.com/harmony-one/harmony/consensus"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/internal/blsgen"
	"github.com/harmony-one/harmony/internal/common"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	shardingconfig "github.com/harmony-one/harmony/internal/configs/sharding"
	"github.com/harmony-one/harmony/internal/ctxerror"
	"github.com/harmony-one/harmony/internal/genesis"
	hmykey "github.com/harmony-one/harmony/internal/keystore"
	"github.com/harmony-one/harmony/internal/memprofiling"
	"github.com/harmony-one/harmony/internal/profiler"
	"github.com/harmony-one/harmony/internal/shardchain"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/node"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/p2p/p2pimpl"
)

var (
	version string
	builtBy string
	builtAt string
	commit  string
)

// InitLDBDatabase initializes a LDBDatabase. isGenesis=true will return the beacon chain database for normal shard nodes
func InitLDBDatabase(ip string, port string, freshDB bool, isBeacon bool) (*ethdb.LDBDatabase, error) {
	var dbFileName string
	if isBeacon {
		dbFileName = fmt.Sprintf("./db/harmony_beacon_%s_%s", ip, port)
	} else {
		dbFileName = fmt.Sprintf("./db/harmony_%s_%s", ip, port)
	}
	if freshDB {
		var err = os.RemoveAll(dbFileName)
		if err != nil {
			fmt.Println(err.Error())
		}
	}
	return ethdb.NewLDBDatabase(dbFileName, 0, 0)
}

func printVersion() {
	fmt.Fprintln(os.Stderr, nodeconfig.GetVersion())
	os.Exit(0)
}

var (
	ip               = flag.String("ip", "127.0.0.1", "ip of the node")
	port             = flag.String("port", "9000", "port of the node.")
	logFolder        = flag.String("log_folder", "latest", "the folder collecting the logs of this execution")
	logMaxSize       = flag.Int("log_max_size", 100, "the max size in megabytes of the log file before it gets rotated")
	freshDB          = flag.Bool("fresh_db", false, "true means the existing disk based db will be removed")
	profile          = flag.Bool("profile", false, "Turn on profiling (CPU, Memory).")
	metricsReportURL = flag.String("metrics_report_url", "", "If set, reports metrics to this URL.")
	versionFlag      = flag.Bool("version", false, "Output version info")
	onlyLogTps       = flag.Bool("only_log_tps", false, "Only log TPS if true")
	dnsZone          = flag.String("dns_zone", "", "if given and not empty, use peers from the zone (default: use libp2p peer discovery instead)")
	dnsFlag          = flag.Bool("dns", true, "[deprecated] equivalent to -dns_zone t.hmny.io")
	//Leader needs to have a minimal number of peers to start consensus
	minPeers = flag.Int("min_peers", 100, "Minimal number of Peers in shard")
	// Key file to store the private key
	keyFile = flag.String("key", "./.hmykey", "the p2p key file of the harmony node")
	// isGenesis indicates this node is a genesis node
	isGenesis = flag.Bool("is_genesis", true, "true means this node is a genesis node")
	// isArchival indicates this node is an archival node that will save and archive current blockchain
	isArchival = flag.Bool("is_archival", true, "false makes node faster by turning caching off")
	// delayCommit is the commit-delay timer, used by Harmony nodes
	delayCommit = flag.String("delay_commit", "0ms", "how long to delay sending commit messages in consensus, ex: 500ms, 1s")
	// isExplorer indicates this node is a node to serve explorer
	isExplorer = flag.Bool("is_explorer", false, "true means this node is a node to serve explorer")
	// networkType indicates the type of the network
	networkType = flag.String("network_type", "mainnet", "type of the network: mainnet, testnet, devnet...")
	// blockPeriod indicates the how long the leader waits to propose a new block.
	blockPeriod = flag.Int("block_period", 8, "how long in second the leader waits to propose a new block.")
	// isNewNode indicates this node is a new node
	isNewNode      = flag.Bool("is_newnode", false, "true means this node is a new node")
	leaderOverride = flag.Bool("leader_override", false, "true means override the default leader role and acts as validator")
	// shardID indicates the shard ID of this node
	shardID            = flag.Int("shard_id", -1, "the shard ID of this node")
	enableMemProfiling = flag.Bool("enableMemProfiling", false, "Enable memsize logging.")
	enableGC           = flag.Bool("enableGC", true, "Enable calling garbage collector manually .")
	blsKeyFile         = flag.String("blskey_file", "", "The encrypted file of bls serialized private key by passphrase.")
	blsPass            = flag.String("blspass", "", "The file containing passphrase to decrypt the encrypted bls file.")
	blsPassphrase      string

	// Sharding configuration parameters for devnet
	devnetNumShards   = flag.Uint("dn_num_shards", 2, "number of shards for -network_type=devnet (default: 2)")
	devnetShardSize   = flag.Int("dn_shard_size", 10, "number of nodes per shard for -network_type=devnet (default 10)")
	devnetHarmonySize = flag.Int("dn_hmy_size", -1, "number of Harmony-operated nodes per shard for -network_type=devnet; negative (default) means equal to -dn_shard_size")

	// logConn logs incoming/outgoing connections
	logConn = flag.Bool("log_conn", false, "log incoming/outgoing connections")

	keystoreDir = flag.String("keystore", hmykey.DefaultKeyStoreDir, "The default keystore directory")

	ks             *keystore.KeyStore
	genesisAccount *genesis.DeployAccount

	// logging verbosity
	verbosity = flag.Int("verbosity", 5, "Logging verbosity: 0=silent, 1=error, 2=warn, 3=info, 4=debug, 5=detail (default: 5)")

	// dbDir is the database directory.
	dbDir = flag.String("db_dir", "", "blockchain database directory")

	// Disable view change.
	disableViewChange = flag.Bool("disable_view_change", false,
		"Do not propose view change (testing only)")
)

func initSetup() (bool, uint32) {

	// maybe request passphrase for bls key.
	passphraseForBls()

	// Configure log parameters
	utils.SetLogContext(*port, *ip)
	utils.SetLogVerbosity(log.Lvl(*verbosity))
	utils.AddLogFile(fmt.Sprintf("%v/validator-%v-%v.log", *logFolder, *ip, *port), *logMaxSize)

	if *onlyLogTps {
		matchFilterHandler := log.MatchFilterHandler("msg", "TPS Report", utils.GetLogInstance().GetHandler())
		utils.GetLogInstance().SetHandler(matchFilterHandler)
	}

	// Add GOMAXPROCS to achieve max performance.
	runtime.GOMAXPROCS(runtime.NumCPU() * 4)

	// Set port and ip to global config.
	nodeconfig.GetDefaultConfig().Port = *port
	nodeconfig.GetDefaultConfig().IP = *ip

	// Setup mem profiling.
	memprofiling.GetMemProfiling().Config()

	// Set default keystore Dir
	hmykey.DefaultKeyStoreDir = *keystoreDir

	// Set up randomization seed.
	rand.Seed(int64(time.Now().Nanosecond()))

	if len(utils.BootNodes) == 0 {
		bootNodeAddrs, err := utils.StringsToAddrs(utils.DefaultBootNodeAddrStrings)
		if err != nil {
			panic(err)
		}
		utils.BootNodes = bootNodeAddrs
	}

	var isLeader bool
	var sid uint32
	if !*isExplorer { // Explorer node doesn't need the following setup
		isLeader, sid = setupECDSAKeys()
	} else {
		genesisAccount = &genesis.DeployAccount{}
		genesisAccount.ShardID = uint32(*shardID)
	}

	// Set up manual call for garbage collection.
	if *enableGC {
		memprofiling.MaybeCallGCPeriodically()
	}
	return isLeader, sid
}

func passphraseForBls() {
	// If FN node running, they should either specify blsPrivateKey or the file with passphrase
	if *blsKeyFile == "" || *blsPass == "" {
		fmt.Println("Internal nodes need to have pass to decrypt blskey")
		os.Exit(101)
	}
	passphrase, err := utils.GetPassphraseFromSource(*blsPass)
	if err != nil {
		fmt.Printf("error when reading passphrase file: %v\n", err)
		os.Exit(100)
	}
	blsPassphrase = passphrase
}

func setupECDSAKeys() (bool, uint32) {
	ks = hmykey.GetHmyKeyStore()

	genesisShardingConfig := core.ShardingSchedule.InstanceForEpoch(big.NewInt(core.GenesisEpoch))
	pubKey := setUpConsensusKey(nodeconfig.GetDefaultConfig())

	var index int
	index, genesisAccount = genesisShardingConfig.FindAccount(pubKey.SerializeToHexStr())
	if index < 0 {
		fmt.Printf("cannot find your BLS key in the genesis/FN tables: %s\n", pubKey.SerializeToHexStr())
		os.Exit(100)
	}

	genesisAccount.ShardID = uint32(index) % genesisShardingConfig.NumShards()

	fmt.Printf("My Genesis Account: %v\n", *genesisAccount)

	// Set up manual call for garbage collection.
	if *enableGC {
		memprofiling.MaybeCallGCPeriodically()
	}
	return index < int(genesisShardingConfig.NumShards()), genesisAccount.ShardID
}

func setUpConsensusKey(nodeConfig *nodeconfig.ConfigType) *bls.PublicKey {
	consensusPriKey, err := blsgen.LoadBlsKeyWithPassPhrase(*blsKeyFile, blsPassphrase)
	if err != nil {
		fmt.Printf("error when loading bls key, err :%v\n", err)
		os.Exit(100)
	}
	pubKey := consensusPriKey.GetPublicKey()

	// Consensus keys are the BLS12-381 keys used to sign consensus messages
	nodeConfig.ConsensusPriKey, nodeConfig.ConsensusPubKey = consensusPriKey, consensusPriKey.GetPublicKey()
	if nodeConfig.ConsensusPriKey == nil || nodeConfig.ConsensusPubKey == nil {
		fmt.Println("error to get consensus keys.")
		os.Exit(100)
	}
	return pubKey
}

func createGlobalConfig(isLeader bool, sid uint32) *nodeconfig.ConfigType {
	var err error
	var myShardID uint32

	nodeConfig := nodeconfig.GetDefaultConfig()

	if !*isExplorer {
		// Specified Shard ID override calculated Shard ID
		if *shardID >= 0 {
			utils.GetLogInstance().Info("ShardID Override", "original", sid, "override", *shardID)
			genesisAccount.ShardID = uint32(*shardID)
		}

		if !*isNewNode {
			nodeConfig = nodeconfig.GetShardConfig(sid)
		} else {
			myShardID = 0 // This should be default value as new node doesn't belong to any shard.
			if *shardID >= 0 {
				utils.GetLogInstance().Info("ShardID Override", "original", myShardID, "override", *shardID)
				myShardID = uint32(*shardID)
				nodeConfig = nodeconfig.GetShardConfig(myShardID)
			}
		}

		// Set up consensus keys.
		setUpConsensusKey(nodeConfig)

		// P2p private key is used for secure message transfer between p2p nodes.
		nodeConfig.P2pPriKey, _, err = utils.LoadKeyFromFile(*keyFile)
		if err != nil {
			panic(err)
		}
	} else {
		nodeConfig = nodeconfig.GetShardConfig(uint32(*shardID))
		nodeConfig.ConsensusPriKey = &bls.SecretKey{} // set dummy bls key for consensus object
	}

	// Set network type
	switch *networkType {
	case nodeconfig.Mainnet:
		nodeConfig.SetNetworkType(nodeconfig.Mainnet)
	case nodeconfig.Testnet:
		nodeConfig.SetNetworkType(nodeconfig.Testnet)
	case nodeconfig.Devnet:
		nodeConfig.SetNetworkType(nodeconfig.Devnet)
	default:
		panic(fmt.Sprintf("invalid network type: %s", *networkType))
	}

	nodeConfig.SelfPeer = p2p.Peer{IP: *ip, Port: *port, ConsensusPubKey: nodeConfig.ConsensusPubKey}

	if isLeader && !*isExplorer && !*leaderOverride { // The first node in a shard is the leader at genesis
		nodeConfig.Leader = nodeConfig.SelfPeer
		nodeConfig.StringRole = "leader"
	} else {
		nodeConfig.StringRole = "validator"
	}

	// P2p private key is used for secure message transfer between p2p nodes.
	nodeConfig.P2pPriKey, _, err = utils.LoadKeyFromFile(*keyFile)
	if err != nil {
		panic(err)
	}

	nodeConfig.Host, err = p2pimpl.NewHost(&nodeConfig.SelfPeer, nodeConfig.P2pPriKey)
	if *logConn && nodeConfig.GetNetworkType() != nodeconfig.Mainnet {
		nodeConfig.Host.GetP2PHost().Network().Notify(utils.NewConnLogger(utils.GetLogInstance()))
	}
	if err != nil {
		panic("unable to new host in harmony")
	}

	if err := nodeConfig.Host.AddPeer(&nodeConfig.Leader); err != nil {
		ctxerror.Warn(utils.GetLogger(), err, "(*p2p.Host).AddPeer failed",
			"peer", &nodeConfig.Leader)
	}

	nodeConfig.DBDir = *dbDir

	return nodeConfig
}

func setUpConsensusAndNode(nodeConfig *nodeconfig.ConfigType) *node.Node {
	// Consensus object.
	// TODO: consensus object shouldn't start here
	// TODO(minhdoan): During refactoring, found out that the peers list is actually empty. Need to clean up the logic of consensus later.
	currentConsensus, err := consensus.New(nodeConfig.Host, nodeConfig.ShardID, nodeConfig.Leader, nodeConfig.ConsensusPriKey)
	currentConsensus.SelfAddress = common.ParseAddr(genesisAccount.Address)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error :%v \n", err)
		os.Exit(1)
	}
	commitDelay, err := time.ParseDuration(*delayCommit)
	if err != nil || commitDelay < 0 {
		_, _ = fmt.Fprintf(os.Stderr, "invalid commit delay %#v", *delayCommit)
		os.Exit(1)
	}
	currentConsensus.SetCommitDelay(commitDelay)
	currentConsensus.MinPeers = *minPeers
	if *disableViewChange {
		currentConsensus.DisableViewChangeForTestingOnly()
	}

	// Current node.
	chainDBFactory := &shardchain.LDBFactory{RootDir: nodeConfig.DBDir}
	currentNode := node.New(nodeConfig.Host, currentConsensus, chainDBFactory, *isArchival)
	if *dnsZone != "" {
		currentNode.SetDNSZone(*dnsZone)
	} else if *dnsFlag {
		currentNode.SetDNSZone("t.hmny.io")
	}
	currentNode.NodeConfig.SetRole(nodeconfig.NewNode)
	// TODO: add staking support
	// currentNode.StakingAccount = myAccount
	utils.GetLogInstance().Info("node account set",
		"address", common.MustAddressToBech32(currentNode.StakingAccount.Address))

	// TODO: refactor the creation of blockchain out of node.New()
	currentConsensus.ChainReader = currentNode.Blockchain()

	// TODO: the setup should only based on shard state
	if *isGenesis {
		// TODO: need change config file and use switch instead of complicated "if else" condition
		if nodeConfig.ShardID == 0 { // Beacon chain
			nodeConfig.SetIsBeacon(true)
			if nodeConfig.StringRole == "leader" {
				currentNode.NodeConfig.SetRole(nodeconfig.BeaconLeader)
				currentNode.NodeConfig.SetIsLeader(true)
			} else {
				currentNode.NodeConfig.SetRole(nodeconfig.BeaconValidator)
				currentNode.NodeConfig.SetIsLeader(false)
			}
			currentNode.NodeConfig.SetShardGroupID(p2p.GroupIDBeacon)
			currentNode.NodeConfig.SetClientGroupID(p2p.GroupIDBeaconClient)
		} else {
			if nodeConfig.StringRole == "leader" {
				currentNode.NodeConfig.SetRole(nodeconfig.ShardLeader)
				currentNode.NodeConfig.SetIsLeader(true)
			} else {
				currentNode.NodeConfig.SetRole(nodeconfig.ShardValidator)
				currentNode.NodeConfig.SetIsLeader(false)
			}
			currentNode.NodeConfig.SetShardGroupID(p2p.NewGroupIDByShardID(p2p.ShardID(nodeConfig.ShardID)))
			currentNode.NodeConfig.SetClientGroupID(p2p.NewClientGroupIDByShardID(p2p.ShardID(nodeConfig.ShardID)))
		}
	} else {
		if *isNewNode {
			currentNode.NodeConfig.SetRole(nodeconfig.NewNode)
			currentNode.NodeConfig.SetClientGroupID(p2p.GroupIDBeaconClient)
			currentNode.NodeConfig.SetBeaconGroupID(p2p.GroupIDBeacon)
			if *shardID > -1 {
				// I will be a validator (single leader is fixed for now)
				currentNode.NodeConfig.SetRole(nodeconfig.ShardValidator)
				currentNode.NodeConfig.SetIsLeader(false)
				currentNode.NodeConfig.SetShardGroupID(p2p.NewGroupIDByShardID(p2p.ShardID(nodeConfig.ShardID)))
				currentNode.NodeConfig.SetClientGroupID(p2p.NewClientGroupIDByShardID(p2p.ShardID(nodeConfig.ShardID)))
			}
		} else if *isExplorer {
			currentNode.NodeConfig.SetRole(nodeconfig.ExplorerNode)
			currentNode.NodeConfig.SetIsLeader(false)
			currentNode.NodeConfig.SetShardGroupID(p2p.NewGroupIDByShardID(p2p.ShardID(*shardID)))
			currentNode.NodeConfig.SetClientGroupID(p2p.NewClientGroupIDByShardID(p2p.ShardID(*shardID)))
		} else if nodeConfig.StringRole == "leader" {
			currentNode.NodeConfig.SetRole(nodeconfig.ShardLeader)
			currentNode.NodeConfig.SetIsLeader(true)
			currentNode.NodeConfig.SetShardGroupID(p2p.GroupIDUnknown)
		} else {
			currentNode.NodeConfig.SetRole(nodeconfig.ShardValidator)
			currentNode.NodeConfig.SetIsLeader(false)
			currentNode.NodeConfig.SetShardGroupID(p2p.GroupIDUnknown)
		}
	}
	currentNode.NodeConfig.ConsensusPubKey = nodeConfig.ConsensusPubKey
	currentNode.NodeConfig.ConsensusPriKey = nodeConfig.ConsensusPriKey

	// Setup block period for currentNode.
	currentNode.BlockPeriod = time.Duration(*blockPeriod) * time.Second

	// TODO: Disable drand. Currently drand isn't functioning but we want to compeletely turn it off for full protection.
	// Enable it back after mainnet.
	// dRand := drand.New(nodeConfig.Host, nodeConfig.ShardID, []p2p.Peer{}, nodeConfig.Leader, currentNode.ConfirmedBlockChannel, nodeConfig.ConsensusPriKey)
	// currentNode.Consensus.RegisterPRndChannel(dRand.PRndChannel)
	// currentNode.Consensus.RegisterRndChannel(dRand.RndChannel)
	// currentNode.DRand = dRand

	// This needs to be executed after consensus and drand are setup
	if !*isNewNode || *shardID > -1 { // initial staking new node doesn't need to initialize shard state
		// TODO: Have a better way to distinguish non-genesis node
		if err := currentNode.InitShardState(*shardID == -1 && !*isNewNode); err != nil {
			ctxerror.Crit(utils.GetLogger(), err, "InitShardState failed",
				"shardID", *shardID, "isNewNode", *isNewNode)
		}
	}

	// Set the consensus ID to be the current block number
	height := currentNode.Blockchain().CurrentBlock().NumberU64()

	currentConsensus.SetViewID(height)
	utils.GetLogInstance().Info("Init Blockchain", "height", height)

	// Assign closure functions to the consensus object
	currentConsensus.BlockVerifier = currentNode.VerifyNewBlock
	currentConsensus.OnConsensusDone = currentNode.PostConsensusProcessing
	currentNode.State = node.NodeWaitToJoin

	// Watching currentNode and currentConsensus.
	memprofiling.GetMemProfiling().Add("currentNode", currentNode)
	memprofiling.GetMemProfiling().Add("currentConsensus", currentConsensus)
	return currentNode
}

func main() {
	flag.Var(&utils.BootNodes, "bootnodes", "a list of bootnode multiaddress (delimited by ,)")
	flag.Parse()

	nodeconfig.SetVersion(fmt.Sprintf("Harmony (C) 2019. %v, version %v-%v (%v %v)", path.Base(os.Args[0]), version, commit, builtBy, builtAt))
	if *versionFlag {
		printVersion()
	}

	switch *networkType {
	case nodeconfig.Mainnet:
		core.ShardingSchedule = shardingconfig.MainnetSchedule
	case nodeconfig.Testnet:
		core.ShardingSchedule = shardingconfig.TestnetSchedule
	case nodeconfig.Devnet:
		if *devnetHarmonySize < 0 {
			*devnetHarmonySize = *devnetShardSize
		}
		devnetConfig, err := shardingconfig.NewInstance(
			uint32(*devnetNumShards), *devnetShardSize, *devnetHarmonySize, genesis.HarmonyAccounts, genesis.FoundationalNodeAccounts)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "invalid devnet sharding config: %s",
				err)
			os.Exit(1)
		}
		core.ShardingSchedule = shardingconfig.NewFixedSchedule(devnetConfig)
	}
	isLeader, sid := initSetup()
	nodeConfig := createGlobalConfig(isLeader, sid)

	// Start Profiler for leader if profile argument is on
	if nodeConfig.StringRole == "leader" && (*profile || *metricsReportURL != "") {
		prof := profiler.GetProfiler()
		prof.Config(nodeConfig.ShardID, *metricsReportURL)
		if *profile {
			prof.Start()
		}
	}
	currentNode := setUpConsensusAndNode(nodeConfig)
	//if consensus.ShardID != 0 {
	//	go currentNode.SupportBeaconSyncing()
	//}

	startMsg := "==== New Harmony Node ===="
	if *isExplorer {
		startMsg = "==== New Explorer Node ===="
	}
	utils.GetLogInstance().Info(startMsg,
		"BlsPubKey", hex.EncodeToString(nodeConfig.ConsensusPubKey.Serialize()),
		"ShardID", nodeConfig.ShardID,
		"ShardGroupID", nodeConfig.GetShardGroupID(),
		"BeaconGroupID", nodeConfig.GetBeaconGroupID(),
		"ClientGroupID", nodeConfig.GetClientGroupID(),
		"Role", currentNode.NodeConfig.Role(),
		"multiaddress", fmt.Sprintf("/ip4/%s/tcp/%s/p2p/%s",
			*ip, *port, nodeConfig.Host.GetID().Pretty()))

	if *enableMemProfiling {
		memprofiling.GetMemProfiling().Start()
	}
	go currentNode.SupportSyncing()
	currentNode.ServiceManagerSetup()

	// RPC for SDK not supported for mainnet.
	if *networkType != nodeconfig.Mainnet {
		if err := currentNode.StartRPC(*port); err != nil {
			ctxerror.Warn(utils.GetLogger(), err, "StartRPC failed")
		}
	}
	currentNode.RunServices()
	currentNode.StartServer()
}
