// Copyright 2017-2021 DERO Project. All rights reserved.
// Use of this source code in any form is governed by RESEARCH license.
// license can be found in the LICENSE file.
// GPG: 0F39 E425 8C65 3947 702A  8234 08B2 0360 A03A 9DE8
//
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY
// EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL
// THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
// PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
// STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF
// THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package blockchain

// This file runs the core consensus protocol
// please think before randomly editing for after effects
// We must not call any packages that can call panic
// NO Panics or FATALs please

import "fmt"
import "sync"
import "time"
import "bytes"
import "runtime/debug"
import "math/big"
import "strings"

//import "runtime"
//import "bufio"
import "golang.org/x/crypto/sha3"
import "github.com/go-logr/logr"

import "sync/atomic"

import "github.com/hashicorp/golang-lru"

import "github.com/deroproject/derohe/rpc"
import "github.com/deroproject/derohe/config"
import "github.com/deroproject/derohe/cryptography/crypto"
import "github.com/deroproject/derohe/errormsg"
import "github.com/deroproject/derohe/metrics"

import "github.com/deroproject/derohe/block"
import "github.com/deroproject/derohe/globals"
import "github.com/deroproject/derohe/transaction"
import "github.com/deroproject/derohe/blockchain/mempool"
import "github.com/deroproject/derohe/blockchain/regpool"

import "github.com/deroproject/graviton"

// all components requiring access to blockchain must use , this struct to communicate
// this structure must be update while mutex
type Blockchain struct {
	Store      storage                     // interface to storage layer
	Height     int64                       // chain height is always 1 more than block
	Top_ID     crypto.Hash                 // id of the top block
	Pruned     int64                       // until where the chain has been pruned
	MiniBlocks *block.MiniBlocksCollection // used for consensus

	Tips map[crypto.Hash]crypto.Hash // current tips

	dag_unsettled              map[crypto.Hash]bool // current unsettled dag
	dag_past_unsettled_cache   *lru.Cache
	dag_future_unsettled_cache *lru.Cache

	mining_blocks_cache          *lru.Cache // used to cache blocks which have been supplied to mining
	cache_IsAddressHashValid     *lru.Cache // used to cache some outputs
	cache_Get_Difficulty_At_Tips *lru.Cache // used to cache some outputs

	integrator_address rpc.Address // integrator rewards will be given to this address

	Difficulty        uint64           // current cumulative difficulty
	Median_Block_Size uint64           // current median block size
	Mempool           *mempool.Mempool // normal tx pool
	Regpool           *regpool.Regpool // registration pool
	Exit_Event        chan bool        // blockchain is shutting down and we must quit ASAP

	Top_Block_Median_Size uint64 // median block size of current top block
	Top_Block_Base_Reward uint64 // top block base reward

	checkpints_disabled bool // are checkpoints disabled
	simulator           bool // is simulator mode

	P2P_Block_Relayer     func(*block.Complete_Block, uint64) // tell p2p to broadcast any block this daemon hash found
	P2P_MiniBlock_Relayer func(mbl block.MiniBlock, peerid uint64)

	RPC_NotifyNewBlock      *sync.Cond // used to notify rpc that a new block has been found
	RPC_NotifyHeightChanged *sync.Cond // used to notify rpc that  chain height has changed due to addition of block
	RPC_NotifyNewMiniBlock  *sync.Cond // used to notify rpc that a new mini block has been found

	Sync bool // whether the sync is active, used while bootstrapping

	sync.RWMutex
}

var logger logr.Logger = logr.Discard() // default discard all logs

// All blockchain activity is store in a single

/* do initialisation , setup storage, put genesis block and chain in store
   This is the first component to get up
   Global parameters are picked up  from the config package
*/

func Blockchain_Start(params map[string]interface{}) (*Blockchain, error) {

	var err error
	var chain Blockchain

	logger = globals.Logger.WithName("CORE")
	logger.V(1).Info("Initialising")

	if err = chain.Store.Initialize(params); err != nil {
		return nil, err
	}
	chain.Tips = map[crypto.Hash]crypto.Hash{}
	chain.MiniBlocks = block.CreateMiniBlockCollection()

	var addr *rpc.Address
	if params["--integrator-address"] == nil {
		if addr, err = rpc.NewAddress(strings.TrimSpace(globals.Config.Dev_Address)); err != nil {
			return nil, err
		}

	} else {
		if addr, err = rpc.NewAddress(strings.TrimSpace(params["--integrator-address"].(string))); err != nil {
			return nil, err
		}
	}
	chain.integrator_address = *addr

	logger.Info("will use", "integrator_address", chain.integrator_address.String())

	//chain.Tips = map[crypto.Hash]crypto.Hash{} // initialize Tips map
	if chain.cache_Get_Difficulty_At_Tips, err = lru.New(8192); err != nil { // temporary cache for difficulty
		return nil, err
	}
	if chain.cache_IsAddressHashValid, err = lru.New(100 * 1024); err != nil { // temporary cache for valid address
		return nil, err
	}
	if chain.mining_blocks_cache, err = lru.New(256); err != nil { // temporary cache for miniing blocks
		return nil, err
	}

	if globals.Arguments["--disable-checkpoints"] != nil {
		chain.checkpints_disabled = globals.Arguments["--disable-checkpoints"].(bool)
	}

	if params["--simulator"] == true {
		chain.simulator = true // enable simulator mode, this will set hard coded difficulty to 1
	}

	chain.Exit_Event = make(chan bool) // init exit channel

	// init mempool before chain starts
	if chain.Mempool, err = mempool.Init_Mempool(params); err != nil {
		return nil, err
	}
	if chain.Regpool, err = regpool.Init_Regpool(params); err != nil {
		return nil, err
	}

	chain.RPC_NotifyNewBlock = sync.NewCond(&sync.Mutex{})      // used by dero daemon to notify all websockets that new block has arrived
	chain.RPC_NotifyHeightChanged = sync.NewCond(&sync.Mutex{}) // used by dero daemon to notify all websockets that chain height has changed
	chain.RPC_NotifyNewMiniBlock = sync.NewCond(&sync.Mutex{})  // used by dero daemon to notify all websockets that new miniblock has arrived

	if !chain.Store.IsBalancesIntialized() {
		logger.Info("Genesis block not in store, add it now")
		var complete_block block.Complete_Block
		bl := Generate_Genesis_Block()
		complete_block.Bl = &bl

		if err, ok := chain.Add_Complete_Block(&complete_block); !ok {
			logger.Error(err, "Failed to add genesis block, we can no longer continue.")
			return nil, err
		}
	}

	init_hard_forks(params) // hard forks must be initialized asap

	chain.Initialise_Chain_From_DB() // load the chain from the disk

	metrics.Version = config.Version.String()
	go metrics.Dump_metrics_data_directly(logger, globals.Arguments["--node-tag"]) // enable metrics if someone needs them

	chain.Sync = true
	if chain.Get_Height() <= 1 {
		if globals.Arguments["--fastsync"] != nil && globals.Arguments["--fastsync"].(bool) {
			chain.Sync = !globals.Arguments["--fastsync"].(bool)
		}
	}

	go clean_up_valid_cache() // clean up valid cache

	atomic.AddUint32(&globals.Subsystem_Active, 1) // increment subsystem

	return &chain, nil
}

// return integrator address
func (chain *Blockchain) IntegratorAddress() rpc.Address {
	return chain.integrator_address
}

// this is the only entrypoint for new / old blocks even for genesis block
// this will add the entire block atomically to the chain
// this is the only function which can add blocks to the chain
// this is exported, so ii can be fed new blocks by p2p layer
// genesis block is no different
// TODO: we should stop mining while adding the new block
func (chain *Blockchain) Add_Complete_Block(cbl *block.Complete_Block) (err error, result bool) {

	var block_hash crypto.Hash
	chain.Lock()
	defer chain.Unlock()
	defer globals.Recover(1)

	bl := cbl.Bl // small pointer to block

	block_hash = bl.GetHash()

	block_logger := logger.WithName(fmt.Sprintf("blid_%s", block_hash)).V(1)
	for k := range chain.Tips { // very fast path
		if block_hash == k {
			return errormsg.ErrAlreadyExists, false // block already in chain skipping it
		}
	}

	// check if block already exist skip it
	if chain.Is_Block_Topological_order(block_hash) {
		return errormsg.ErrAlreadyExists, false // block already in chain skipping it
	}

	result = false
	height_changed := false

	processing_start := time.Now()

	//old_top := chain.Load_TOP_ID() // store top as it may change
	defer func() {

		// safety so if anything wrong happens, verification fails
		if r := recover(); r != nil {
			logger.V(1).Error(r.(error), "Recovered while adding new block", "blid", block_hash, "stack", fmt.Sprintf("%s", string(debug.Stack())))
			result = false
			err = errormsg.ErrPanic
		}

		if result == true { // block was successfully added, commit it atomically
			logger.V(2).Info("Block successfully accepted by chain", "blid", block_hash.String())

			// gracefully try to instrument
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.V(1).Error(r.(error), "Recovered while instrumenting", "stack", debug.Stack())
					}
				}()
				metrics.Set.GetOrCreateCounter("blockchain_tx_total").Add(len(cbl.Bl.Tx_hashes))
				metrics.Set.GetOrCreateHistogram("block_txcount_histogram_short").Update(float64(len(cbl.Bl.Tx_hashes)))
				metrics.Set.GetOrCreateHistogram("block_processing_duration_histogram_seconds").UpdateDuration(processing_start)

				// tracks counters for tx internals, do we need to serialize everytime, just for stats
				{
					complete_block_size := 0
					for i := 0; i < len(cbl.Txs); i++ {
						tx_size := len(cbl.Txs[i].Serialize())
						complete_block_size += tx_size
						metrics.Set.GetOrCreateHistogram("transaction_size_histogram_bytes").Update(float64(tx_size))
						metrics.Set.GetOrCreateCounter(fmt.Sprintf(`transaction_total{type="%s"}`, cbl.Txs[i].TransactionType.String())).Inc()
						if len(cbl.Txs[i].Payloads) >= 1 {
							metrics.Set.GetOrCreateHistogram("transaction_ring_histogram_short").Update(float64(cbl.Txs[i].Payloads[0].Statement.RingSize))
							metrics.Set.GetOrCreateHistogram("transaction_payloads_histogram_short").Update(float64(len(cbl.Txs[i].Payloads)))
						}
					}
					metrics.Set.GetOrCreateHistogram("block_size_histogram_bytes").Update(float64(complete_block_size))
				}
			}()

			// notify everyone who needs to know that a new block is in the chain
			chain.RPC_NotifyNewBlock.L.Lock()
			chain.RPC_NotifyNewBlock.Broadcast()
			chain.RPC_NotifyNewBlock.L.Unlock()

			if height_changed {
				chain.RPC_NotifyHeightChanged.L.Lock()
				chain.RPC_NotifyHeightChanged.Broadcast()
				chain.RPC_NotifyHeightChanged.L.Unlock()
			}

		} else {

			logger.V(1).Error(err, "Block rejected by chain", "BLID", block_hash)
		}
	}()

	// first of all lets do some quick checks
	// before doing extensive checks
	result = false

	// check if block already exist skip it
	if chain.Is_Block_Topological_order(block_hash) {
		return errormsg.ErrAlreadyExists, false // block already in chain skipping it
	}

	for k := range chain.Tips {
		if block_hash == k {
			return errormsg.ErrAlreadyExists, false // block already in chain skipping it
		}
	}

	// only 2 tips allowed in block
	if len(bl.Tips) >= 3 {
		block_logger.V(1).Error(fmt.Errorf("More than 2 tips present in block rejecting"), "")
		return errormsg.ErrPastMissing, false
	}

	// check whether the tips exist in our chain, if not reject
	for i := range bl.Tips {
		if !chain.Block_Exists(bl.Tips[i]) { // alt-tips might not have a topo order at this point, so make sure they exist on disk
			block_logger.V(1).Error(fmt.Errorf("Tip is NOT present in chain, skipping it till we get a parent"), "", "missing_tip", bl.Tips[i].String())
			return errormsg.ErrPastMissing, false
		}
	}

	block_height := chain.Calculate_Height_At_Tips(bl.Tips)
	for i := range bl.Tips { // previous block can be refer to only recent blocks, making some attacks almost impossible
		if block_height != chain.Load_Block_Height(bl.Tips[i])+1 {
			block_logger.V(1).Error(fmt.Errorf("Block  rejected since it is in too past"), "", "block_height", block_height, "tip_height", chain.Load_Block_Height(bl.Tips[i]))
			return errormsg.ErrInvalidBlock, false
		}
	}

	if block_height == 0 && int64(bl.Height) == block_height && len(bl.Tips) != 0 {
		block_logger.Error(fmt.Errorf("Genesis block cannot have tips."), "", "tip_count", len(bl.Tips))
		return errormsg.ErrInvalidBlock, false
	}

	if len(bl.Tips) >= 1 && bl.Height == 0 {
		block_logger.Error(fmt.Errorf("Genesis block can only be at height 0"), "", "tip_count", len(bl.Tips))
		return errormsg.ErrInvalidBlock, false
	}

	if block_height != 0 && block_height < chain.Get_Stable_Height() {
		block_logger.Error(fmt.Errorf("Block rejected since it is stale."), "", "stable height", chain.Get_Stable_Height(), "block height", block_height)
		return errormsg.ErrInvalidBlock, false
	}

	// make sure time is NOT into future,
	// if clock diff is more than  50 millisecs, reject the block
	if bl.Timestamp > (uint64(globals.Time().UTC().UnixMilli() + 50)) { // give 50 millisec passing
		block_logger.Error(fmt.Errorf("Rejecting Block, timestamp is too much into future, make sure that system clock is correct"), "")
		return errormsg.ErrFutureTimestamp, false
	}

	// verify that the clock is not being run in reverse
	// the block timestamp cannot be less than any of the parents
	for i := range bl.Tips {
		if chain.Load_Block_Timestamp(bl.Tips[i]) > bl.Timestamp {
			fmt.Printf("timestamp prev %d  cur timestamp %d\n", chain.Load_Block_Timestamp(bl.Tips[i]), bl.Timestamp)

			block_logger.Error(fmt.Errorf("Block timestamp is  less than its parent."), "rejecting block")
			return errormsg.ErrInvalidTimestamp, false
		}
	}

	// check whether the major version ( hard fork) is valid
	if !chain.Check_Block_Version(bl) {
		block_logger.Error(fmt.Errorf("Rejecting !! Block has invalid fork version"), "actual", bl.Major_Version, "expected", chain.Get_Current_Version_at_Height(chain.Calculate_Height_At_Tips(bl.Tips)))
		return errormsg.ErrInvalidBlock, false
	}

	// verify whether the tips are reachable from one another
	if bl.Height >= 2 && !chain.CheckDagStructure(bl.Tips) {
		block_logger.Error(fmt.Errorf("Rejecting !! Block has invalid reachability"), "Invalid rechability", "tips", bl.Tips)
		return errormsg.ErrInvalidBlock, false

	}

	// if the block is referencing any past tip too distant into its history
	for i := range bl.Tips {
		if int64(bl.Height)-1 != chain.Load_Block_Height(bl.Tips[i]) {
			block_logger.Error(fmt.Errorf("Rusty TIP  mined by ROGUE miner discarding block"), "", "best height", bl.Height, "deviation", int64(bl.Height)-chain.Load_Block_Height(bl.Tips[i]))
			return errormsg.ErrInvalidBlock, false
		}
	}

	// check whether the block crosses the size limit
	// block size is calculate by adding all the txs
	// block header/miner tx is excluded, only tx size if calculated
	{
		block_size := 0
		for i := 0; i < len(cbl.Txs); i++ {
			block_size += len(cbl.Txs[i].Serialize())
			if uint64(block_size) >= config.STARGATE_HE_MAX_BLOCK_SIZE {
				block_logger.Error(fmt.Errorf("Block is bigger than max permitted"), "Rejecting", "Actual", block_size, "MAX", config.STARGATE_HE_MAX_BLOCK_SIZE)
				return errormsg.ErrInvalidSize, false
			}
		}
	}

	// verify everything related to miniblocks in one go
	{
		if err = chain.Verify_MiniBlocks(*cbl.Bl); err != nil {
			return err, false
		}

		if bl.Height != 0 { // a genesis block doesn't have miniblock

			// verify hash of miniblock for corruption
			if err = chain.Verify_MiniBlocks_HashCheck(cbl); err != nil {
				return err, false
			}

			// check dynamic consensus rules
			if err = chain.Check_Dynamism(cbl.Bl.MiniBlocks); err != nil {
				return err, false
			}
		}

		for _, mbl := range bl.MiniBlocks {
			var miner_hash crypto.Hash
			copy(miner_hash[:], mbl.KeyHash[:])
			if !chain.IsAddressHashValid(miner_hash) {
				err = fmt.Errorf("miner address not registered")
				return err, false
			}
		}

		// verify Pow of miniblocks
		for i, mbl := range bl.MiniBlocks {
			if !chain.VerifyMiniblockPoW(bl, mbl) {
				block_logger.Error(fmt.Errorf("MiniBlock has invalid PoW"), "rejecting", "i", i)
				return errormsg.ErrInvalidPoW, false
			}
		}
	}

	{ // miner TX checks are here
		if bl.Height == 0 && !bl.Miner_TX.IsPremine() { // genesis block contain premine tx a
			block_logger.Error(fmt.Errorf("Miner tx failed verification for genesis"), "rejecting")
			return errormsg.ErrInvalidBlock, false
		}

		if bl.Height != 0 && !bl.Miner_TX.IsCoinbase() { // all blocks except genesis block contain coinbase TX
			block_logger.Error(fmt.Errorf("Miner tx failed  it is not coinbase"), "rejecting")
			return errormsg.ErrInvalidBlock, false
		}

		// always check whether the coin base tx is okay
		if bl.Height != 0 {
			if err = chain.Verify_Transaction_Coinbase(cbl, &bl.Miner_TX); err != nil { // if miner address is not registered give error
				//block_logger.Warnf("Error verifying coinbase tx, err :'%s'", err)
				return err, false
			}
		}

		// TODO we need to verify address  whether they are valid points on curve or not
	}

	// now we need to verify each and every tx in detail
	// we need to verify each and every tx contained in the block, sanity check everything
	// first of all check, whether all the tx contained in the block, match their hashes
	{
		if len(bl.Tx_hashes) != len(cbl.Txs) {
			block_logger.Error(fmt.Errorf("Missing TX"), "Incomplete block", "expected_tx", len(bl.Tx_hashes), "actual_tx", len(cbl.Txs))
			return errormsg.ErrInvalidBlock, false
		}

		// first check whether the complete block contains any diplicate hashes
		tx_checklist := map[crypto.Hash]bool{}
		for i := 0; i < len(bl.Tx_hashes); i++ {
			tx_checklist[bl.Tx_hashes[i]] = true
		}

		if len(tx_checklist) != len(bl.Tx_hashes) { // block has duplicate tx, reject
			block_logger.Error(fmt.Errorf("duplicate TX"), "Incomplete block", "duplicate count", len(bl.Tx_hashes)-len(tx_checklist))
			return errormsg.ErrInvalidBlock, false

		}
		// now lets loop through complete block, matching each tx
		// detecting any duplicates using txid hash
		for i := 0; i < len(cbl.Txs); i++ {
			tx_hash := cbl.Txs[i].GetHash()
			if _, ok := tx_checklist[tx_hash]; !ok {
				// tx is NOT found in map, RED alert reject the block
				block_logger.Error(fmt.Errorf("Missing TX"), "TX missing", "txid", tx_hash.String())
				return errormsg.ErrInvalidBlock, false
			}
		}
	}

	// another check, whether the block contains any duplicate registration within the block
	// block wide duplicate input detector
	{
		reg_map := map[string]bool{}
		for i := 0; i < len(cbl.Txs); i++ {

			if cbl.Txs[i].TransactionType == transaction.REGISTRATION {
				if _, ok := reg_map[string(cbl.Txs[i].MinerAddress[:])]; ok {
					block_logger.Error(fmt.Errorf("Double Registration TX"), "duplicate registration", "txid", cbl.Txs[i].GetHash())
					return errormsg.ErrTXDoubleSpend, false
				}
				reg_map[string(cbl.Txs[i].MinerAddress[:])] = true
			}
		}
	}

	// another check, whether the tx contains any duplicate nonces within the block
	// block wide duplicate input detector
	{
		nonce_map := map[crypto.Hash]bool{}
		for i := 0; i < len(cbl.Txs); i++ {

			if cbl.Txs[i].TransactionType == transaction.NORMAL || cbl.Txs[i].TransactionType == transaction.BURN_TX || cbl.Txs[i].TransactionType == transaction.SC_TX {
				for j := range cbl.Txs[i].Payloads {
					if _, ok := nonce_map[cbl.Txs[i].Payloads[j].Proof.Nonce()]; ok {
						block_logger.Error(fmt.Errorf("Double Spend TX within block"), "duplicate nonce", "txid", cbl.Txs[i].GetHash())
						return errormsg.ErrTXDoubleSpend, false
					}
					nonce_map[cbl.Txs[i].Payloads[j].Proof.Nonce()] = true
				}

			}
		}
	}

	// we need to verify each tx with tips
	{
		fail_count := int32(0)
		wg := sync.WaitGroup{}
		wg.Add(len(cbl.Txs)) // add total number of tx as work

		hf_version := chain.Get_Current_Version_at_Height(chain.Calculate_Height_At_Tips(bl.Tips))
		for i := 0; i < len(cbl.Txs); i++ {
			go func(j int) {
				if err := chain.Verify_Transaction_NonCoinbase_CheckNonce_Tips(hf_version, cbl.Txs[j], bl.Tips, false); err != nil { // transaction verification failed
					atomic.AddInt32(&fail_count, 1) // increase fail count by 1
					block_logger.Error(err, "tx nonce verification failed", "txid", cbl.Txs[j].GetHash())
				}
				wg.Done()
			}(i)
		}

		wg.Wait()           // wait for verifications to finish
		if fail_count > 0 { // check the result
			block_logger.Error(fmt.Errorf("TX nonce verification failed"), "rejecting block")
			return errormsg.ErrInvalidTX, false
		}
	}

	// we need to anyways verify the TXS since proofs are not covered by checksum
	{
		fail_count := int32(0)
		wg := sync.WaitGroup{}
		wg.Add(len(cbl.Txs)) // add total number of tx as work

		hf_version := chain.Get_Current_Version_at_Height(chain.Calculate_Height_At_Tips(bl.Tips))
		for i := 0; i < len(cbl.Txs); i++ {
			go func(j int) {
				if err := chain.Verify_Transaction_NonCoinbase(hf_version, cbl.Txs[j]); err != nil { // transaction verification failed
					atomic.AddInt32(&fail_count, 1) // increase fail count by 1
					block_logger.Error(err, "tx verification failed", "txid", cbl.Txs[j].GetHash())
				}
				wg.Done()
			}(i)
		}

		wg.Wait()           // wait for verifications to finish
		if fail_count > 0 { // check the result
			block_logger.Error(fmt.Errorf("TX verification failed"), "rejecting block")
			return errormsg.ErrInvalidTX, false
		}
	}

	// we need to do more checks but only after tx has been expanded
	{
		var check_data cbl_verify // used to verify sanity of new block
		for i := 0; i < len(cbl.Txs); i++ {
			if !(cbl.Txs[i].IsCoinbase() || cbl.Txs[i].IsRegistration()) { // all other tx must go through this check
				if err = check_data.check(cbl.Txs[i], false); err == nil {
					check_data.check(cbl.Txs[i], true) // keep in record for future tx
				} else {
					block_logger.Error(err, "Invalid TX within block", "txid", cbl.Txs[i].GetHash())
					return errormsg.ErrInvalidTX, false
				}
			}
		}
	}

	// we are here means everything looks good, proceed and save to chain
	//skip_checks:

	// save all the txs
	// and then save the block
	{ // first lets save all the txs, together with their link to this block as height
		for i := 0; i < len(cbl.Txs); i++ {
			if err = chain.Store.Block_tx_store.WriteTX(bl.Tx_hashes[i], cbl.Txs[i].Serialize()); err != nil {
				panic(err)
			}
		}
	}

	chain.StoreBlock(bl)

	// if the block is on a lower height tip, the block will not increase chain height
	height := chain.Load_Height_for_BL_ID(block_hash)
	if height > chain.Get_Height() || height == 0 { // exception for genesis block
		atomic.StoreInt64(&chain.Height, height)
		//chain.Store_TOP_HEIGHT(dbtx, height)

		height_changed = true
		block_logger.Info("Chain extended", "new height", chain.Height)
	} else {
		block_logger.Info("Chain extended but height is same", "new height", chain.Height)

	}

	// process tips only if increases the the height
	if height_changed {

		var full_order []crypto.Hash
		var base_topo_index int64 // new topo id will start from here

		if cbl.Bl.Height == 0 {
			full_order = append(full_order, cbl.Bl.GetHash())
		} else {
			current_tip := chain.Get_Top_ID()
			new_tip := cbl.Bl.GetHash()
			full_order, base_topo_index = chain.Generate_Full_Order_New(current_tip, new_tip)
		}

		// we will directly use graviton to mov in to history
		logger.V(3).Info("Full order data", "full_order", full_order, "base_topo_index", base_topo_index)

		for i := int64(0); i < int64(len(full_order)); i++ {
			logger.V(3).Info("will execute order ", "i", i, "blid", full_order[i].String())

			current_topo_block := i + base_topo_index
			previous_topo_block := current_topo_block - 1

			if current_topo_block == chain.Load_Block_Topological_order(full_order[i]) { // skip if same order
				continue
			}

			// TODO we must run smart contracts and TXs in this order
			// basically client protocol must run here
			// even if the HF has triggered we may still accept, old blocks for some time
			// so hf is detected block-wise and processed as such

			bl_current_hash := full_order[i]
			bl_current, err1 := chain.Load_BL_FROM_ID(bl_current_hash)
			if err1 != nil {
				logger.Error(err, "Cannot load block  for client protocol,probably DB corruption", "blid", bl_current_hash.String())
				return errormsg.ErrInvalidBlock, false
			}

			//fmt.Printf("\ni %d bl %+v\n",i, bl_current)

			height_current := chain.Calculate_Height_At_Tips(bl_current.Tips)
			hard_fork_version_current := chain.Get_Current_Version_at_Height(height_current)

			// this version does not require client protocol as of now
			//  run full client protocol and find valid transactions
			//	rlog.Debugf("running client protocol for %s minertx %s  topo %d", bl_current_hash, bl_current.Miner_TX.GetHash(), highest_topo)

			// generate miner TX rewards as per client protocol
			if hard_fork_version_current == 1 {

			}

			var balance_tree, sc_meta *graviton.Tree
			_ = sc_meta

			var ss *graviton.Snapshot
			if bl_current.Height == 0 { // if it's genesis block
				if ss, err = chain.Store.Balance_store.LoadSnapshot(0); err != nil {
					panic(err)
				} else if balance_tree, err = ss.GetTree(config.BALANCE_TREE); err != nil {
					panic(err)
				} else if sc_meta, err = ss.GetTree(config.SC_META); err != nil {
					panic(err)
				}
			} else { // we already have a block before us, use it

				record_version := uint64(0)
				if previous_topo_block >= 0 {
					toporecord, err := chain.Store.Topo_store.Read(previous_topo_block)

					if err != nil {
						panic(err)
					}
					record_version = toporecord.State_Version
				}

				ss, err = chain.Store.Balance_store.LoadSnapshot(record_version)
				if err != nil {
					panic(err)
				}

				if balance_tree, err = ss.GetTree(config.BALANCE_TREE); err != nil {
					panic(err)
				}
				if sc_meta, err = ss.GetTree(config.SC_META); err != nil {
					panic(err)
				}
			}

			fees_collected := uint64(0)

			// side blocks only represent chain strenth , else they are are ignored
			// this means they donot get any reward , 0 reward
			// their transactions are ignored

			//chain.Store.Topo_store.Write(i+base_topo_index, full_order[i],0, int64(bl_current.Height)) // write entry so as sideblock could work
			var data_trees []*graviton.Tree

			if !chain.isblock_SideBlock_internal(full_order[i], current_topo_block, int64(bl_current.Height)) {

				sc_change_cache := map[crypto.Hash]*graviton.Tree{} // cache entire changes for entire block

				// install hardcoded contracts
				if err = chain.install_hardcoded_contracts(sc_change_cache, ss, balance_tree, sc_meta, bl_current.Height); err != nil {
					panic(err)
				}

				for _, txhash := range bl_current.Tx_hashes { // execute all the transactions
					if tx_bytes, err := chain.Store.Block_tx_store.ReadTX(txhash); err != nil {
						panic(err)
					} else {
						var tx transaction.Transaction
						if err = tx.Deserialize(tx_bytes); err != nil {
							panic(err)
						}
						for t := range tx.Payloads {
							if !tx.Payloads[t].SCID.IsZero() {
								tree, _ := ss.GetTree(string(tx.Payloads[t].SCID[:]))
								sc_change_cache[tx.Payloads[t].SCID] = tree
							}
						}
						// we have loaded a tx successfully, now lets execute it
						tx_fees := chain.process_transaction(sc_change_cache, tx, balance_tree, bl_current.Height)

						//fmt.Printf("transaction %s type %s data %+v\n", txhash, tx.TransactionType, tx.SCDATA)
						if tx.TransactionType == transaction.SC_TX {
							tx_fees, err = chain.process_transaction_sc(sc_change_cache, ss, bl_current.Height, uint64(current_topo_block), bl_current.Timestamp/1000, bl_current_hash, tx, balance_tree, sc_meta)

							//fmt.Printf("Processsing sc err %s\n", err)
							if err == nil { // TODO process gasg here

							}
						}
						fees_collected += tx_fees
					}
				}

				// at this point, we must commit all the SCs, so entire tree hash is interlinked
				for scid, v := range sc_change_cache {
					meta_bytes, err := sc_meta.Get(SC_Meta_Key(scid))
					if err != nil {
						panic(err)
					}

					var meta SC_META_DATA // the meta contains metadata about SC
					if err := meta.UnmarshalBinary(meta_bytes); err != nil {
						panic(err)
					}

					if meta.DataHash, err = v.Hash(); err != nil { // encode data tree hash
						panic(err)
					}

					sc_meta.Put(SC_Meta_Key(scid), meta.MarshalBinary())
					data_trees = append(data_trees, v)

					/*fmt.Printf("will commit tree name %x \n", v.GetName())
									c := v.Cursor()
						for k, v, err := c.First(); err == nil; k, v, err = c.Next() {
						fmt.Printf("key=%x, value=%x\n", k, v)
					}*/

				}

				chain.process_miner_transaction(bl_current, bl_current.Height == 0, balance_tree, fees_collected, bl_current.Height)
			} else {
				block_logger.V(1).Info("this block is a side block", "height", chain.Load_Block_Height(full_order[i]), "blid", full_order[i])

			}

			// we are here, means everything is okay, lets commit the update balance tree

			data_trees = append(data_trees, balance_tree, sc_meta)

			//fmt.Printf("committing data trees %+v\n", data_trees)

			commit_version, err := graviton.Commit(data_trees...)
			if err != nil {
				panic(err)
			}

			//fmt.Printf("committed trees version  %d at topo %d\n", commit_version, current_topo_block)

			chain.Store.Topo_store.Write(current_topo_block, full_order[i], commit_version, chain.Load_Block_Height(full_order[i]))

			//rlog.Debugf("%d %s   topo_index %d  base topo %d", i, full_order[i], current_topo_block, base_topo_index)

			// this tx must be stored, linked with this block

		}
	}

	{

		// calculate new set of tips
		// this is done by removing all known tips which are in the past
		// and add this block as tip

		old_tips := chain.Get_TIPS()
		var tips []crypto.Hash
		new_tips := map[crypto.Hash]crypto.Hash{}

		for i := range old_tips {
			for j := range bl.Tips {
				if bl.Tips[j] == old_tips[i] {
					goto skip_tip
				}
			}
			tips = append(tips, old_tips[i])
		skip_tip:
		}

		tips = append(tips, bl.GetHash()) // add current block as new tip

		chain_height := chain.Get_Height()

		for i := range tips {
			tip_height := int64(chain.Load_Height_for_BL_ID(tips[i]))
			if (chain_height - tip_height) < 2 {

				new_tips[tips[i]] = tips[i]
			} else { // this should be a rare event, unless network has very high latency
				logger.V(2).Info("Rusty TIP declared stale", "tip", tips[i], "best height", chain_height, "tip_height", tip_height)
				//chain.transaction_scavenger(dbtx, tips[i]) // scavenge tx if possible
				// TODO we must include any TX from the orphan blocks back to the mempool to avoid losing any TX
			}
		}

		//block_logger.Info("New tips(after adding block) ", "tips", new_tips)

		chain.Tips = new_tips
	}

	// every 2000 block print a line
	if chain.Get_Height()%2000 == 0 {
		block_logger.Info(fmt.Sprintf("Chain Height %d", chain.Height))
	}

	result = true

	// TODO fix hard fork
	// maintain hard fork votes to keep them SANE
	//chain.Recount_Votes() // does not return anything

	// enable mempool book keeping

	func() {
		if r := recover(); r != nil {
			logger.Error(r.(error), "Mempool House Keeping triggered panic", "height", block_height)
		}

		purge_count := chain.MiniBlocks.PurgeHeight(chain.Get_Stable_Height()) // purge all miniblocks upto this height
		logger.V(2).Info("Purged miniblock", "count", purge_count)

		// discard the transactions from mempool if they are present there
		chain.Mempool.Monitor()

		for i := 0; i < len(cbl.Txs); i++ {
			txid := cbl.Txs[i].GetHash()

			switch cbl.Txs[i].TransactionType {

			case transaction.REGISTRATION:
				if chain.Regpool.Regpool_TX_Exist(txid) {
					logger.V(3).Info("Deleting TX from regpool", "txid", txid)
					chain.Regpool.Regpool_Delete_TX(txid)
					continue
				}

			case transaction.NORMAL, transaction.BURN_TX, transaction.SC_TX:
				if chain.Mempool.Mempool_TX_Exist(txid) {
					logger.V(3).Info("Deleting TX from mempool", "txid", txid)
					chain.Mempool.Mempool_Delete_TX(txid)
					continue
				}

			}

		}

		// give mempool an oppurtunity to clean up tx, but only if they are not mined
		chain.Mempool.HouseKeeping(uint64(block_height))

		// give regpool a chance to register
		if ss, err := chain.Store.Balance_store.LoadSnapshot(0); err == nil {
			if balance_tree, err := ss.GetTree(config.BALANCE_TREE); err == nil {

				chain.Regpool.HouseKeeping(uint64(block_height), func(tx *transaction.Transaction) bool {
					if tx.TransactionType != transaction.REGISTRATION { // tx not registration so delete
						return true
					}
					if _, err := balance_tree.Get(tx.MinerAddress[:]); err != nil { // address already registered
						return true
					}
					return false // account not already registered, so give another chance
				})

			}
		}

	}()

	return // run any handlers necesary to atomically
}

// this function is called to read blockchain state from DB
// It is callable at any point in time

func (chain *Blockchain) Initialise_Chain_From_DB() {
	chain.Lock()
	defer chain.Unlock()

	chain.Pruned = chain.LocatePruneTopo()
	if chain.Pruned >= 1 {
		logger.Info("Chain Pruned till", "topoheight", chain.Pruned)
	}

	// find the tips from the chain , first by reaching top height
	// then downgrading to top-10 height
	// then reworking the chain to get the tip
	best_height := chain.Load_TOP_HEIGHT()
	chain.Height = best_height

	chain.Tips = map[crypto.Hash]crypto.Hash{} // reset the map
	// reload top tip from disk
	top := chain.Get_Top_ID()

	chain.Tips[top] = top // we only can load a single tip from db

	// get dag unsettled, it's only possible when we have the tips
	// chain.dag_unsettled = chain.Get_DAG_Unsettled() // directly off the disk

	logger.V(1).Info("Reloaded Chain from disk", "Tips", chain.Tips, "Height", chain.Height)
}

// before shutdown , make sure p2p is confirmed stopped
func (chain *Blockchain) Shutdown() {

	chain.Lock()            // take the lock as chain is no longer in unsafe mode
	close(chain.Exit_Event) // send signal to everyone we are shutting down

	chain.Mempool.Shutdown() // shutdown mempool first
	chain.Regpool.Shutdown() // shutdown regpool first

	logger.Info("Stopping Blockchain")
	//chain.Store.Shutdown()
	atomic.AddUint32(&globals.Subsystem_Active, ^uint32(0)) // this decrement 1 fom subsystem
}

// get top unstable height
// this is obtained by  getting the highest topo block and getting its height
func (chain *Blockchain) Get_Height() int64 {
	topo_count := chain.Store.Topo_store.Count()
	if topo_count == 0 {
		return 0
	}

	//return atomic.LoadUint64(&chain.Height)
	return chain.Load_TOP_HEIGHT()
}

// get height where chain is now stable
func (chain *Blockchain) Get_Stable_Height() int64 {
	return chain.Get_Height() - config.STABLE_LIMIT
}

// we should be holding lock at this time, atleast read only

func (chain *Blockchain) Get_TIPS() (tips []crypto.Hash) {
	for _, x := range chain.Tips {
		tips = append(tips, x)

	}
	return tips
}

func (chain *Blockchain) Get_Difficulty() uint64 {
	return chain.Get_Difficulty_At_Tips(chain.Get_TIPS()).Uint64()
}

/*
func (chain *Blockchain) Get_Cumulative_Difficulty() uint64 {

	return 0 //chain.Load_Block_Cumulative_Difficulty(chain.Top_ID)
}

func (chain *Blockchain) Get_Median_Block_Size() uint64 { // get current cached median size
	return chain.Median_Block_Size
}
*/
func (chain *Blockchain) Get_Network_HashRate() uint64 {
	return chain.Get_Difficulty()
}

// this is used to for quick syncs as entire blocks as SHA1,
// entires block can skipped for verification, if checksum matches what the devs have stored
func (chain *Blockchain) BlockCheckSum(cbl *block.Complete_Block) []byte {
	h := sha3.New256()
	h.Write(cbl.Bl.Serialize())
	for i := range cbl.Txs {
		h.Write(cbl.Txs[i].Serialize())
	}
	return h.Sum(nil)
}

// this is the only entrypoint for new txs in the chain
// add a transaction to MEMPOOL,
// verifying everything  means everything possible
// this only change mempool, no DB changes
func (chain *Blockchain) Add_TX_To_Pool(tx *transaction.Transaction) error {
	var err error

	if tx.IsPremine() {
		return fmt.Errorf("premine tx not mineable")
	}
	if tx.IsRegistration() { // registration tx will not go any forward
		// ggive regpool a chance to register
		if ss, err := chain.Store.Balance_store.LoadSnapshot(0); err == nil {
			if balance_tree, err := ss.GetTree(config.BALANCE_TREE); err == nil {
				if _, err := balance_tree.Get(tx.MinerAddress[:]); err == nil { // address already registered
					return fmt.Errorf("address already registered")
				} else { // add  to regpool
					if chain.Regpool.Regpool_Add_TX(tx, 0) {
						return nil
					} else {
						return fmt.Errorf("registration for address is already pending")
					}
				}
			} else {
				return err
			}
		} else {
			return err
		}
	}

	switch tx.TransactionType {
	case transaction.BURN_TX, transaction.NORMAL, transaction.SC_TX:
	default:
		return fmt.Errorf("such transaction type cannot appear in mempool")
	}

	txhash := tx.GetHash()

	// Coin base TX can not come through this path
	if tx.IsCoinbase() {
		logger.Error(fmt.Errorf("coinbase tx cannot appear in mempool"), "tx_rejected", "txid", txhash)
		return fmt.Errorf("TX rejected  coinbase tx cannot appear in mempool")
	}

	chain_height := uint64(chain.Get_Height())
	/*if chain_height > tx.Height {
		rlog.Tracef(2, "TX %s rejected since chain has already progressed", txhash)
		return fmt.Errorf("TX %s rejected since chain has already progressed", txhash)
	}*/

	// quick check without calculating everything whether tx is in pool, if yes we do nothing
	if chain.Mempool.Mempool_TX_Exist(txhash) {
		//rlog.Tracef(2, "TX %s rejected Already in MEMPOOL", txhash)
		return fmt.Errorf("TX %s rejected Already in MEMPOOL", txhash)
	}

	// check whether tx is already mined
	if _, err = chain.Store.Block_tx_store.ReadTX(txhash); err == nil {
		//rlog.Tracef(2, "TX %s rejected Already mined in some block", txhash)
		return fmt.Errorf("TX %s rejected Already mined in some block", txhash)
	}

	hf_version := chain.Get_Current_Version_at_Height(int64(chain_height))

	// if TX is too big, then it cannot be mined due to fixed block size, reject such TXs here
	// currently, limits are  as per consensus
	if uint64(len(tx.Serialize())) > config.STARGATE_HE_MAX_TX_SIZE {
		logger.Error(fmt.Errorf("Huge TX"), "TX rejected", "Actual Size", len(tx.Serialize()), "max possible ", config.STARGATE_HE_MAX_TX_SIZE)
		return fmt.Errorf("TX rejected  Size %d byte Max possible %d", len(tx.Serialize()), config.STARGATE_HE_MAX_TX_SIZE)
	}

	// check whether enough fees is provided in the transaction
	calculated_fee := chain.Calculate_TX_fee(hf_version, uint64(len(tx.Serialize())))
	provided_fee := tx.Fees() // get fee from tx

	//logger.WithFields(log.Fields{"txid": txhash}).Warnf("TX fees check disabled  provided fee %d calculated fee %d", provided_fee, calculated_fee)
	if calculated_fee > provided_fee {
		err = fmt.Errorf("TX  %s rejected due to low fees  provided fee %d calculated fee %d", txhash, provided_fee, calculated_fee)
		return err
	}

	if err := chain.Verify_Transaction_NonCoinbase_CheckNonce_Tips(hf_version, tx, chain.Get_TIPS(), true); err != nil { // transaction verification failed
		logger.V(1).Error(err, "Incoming TX nonce verification failed", "txid", txhash)
		return fmt.Errorf("Incoming TX %s nonce verification failed, err %s", txhash, err)
	}

	if err := chain.Verify_Transaction_NonCoinbase(hf_version, tx); err != nil {
		logger.V(1).Error(err, "Incoming TX could not be verified", "txid", txhash)
		return fmt.Errorf("Incoming TX %s could not be verified, err %s", txhash, err)
	}

	if chain.Mempool.Mempool_Add_TX(tx, 0) { // new tx come with 0 marker
		//rlog.Tracef(2, "Successfully added tx %s to pool", txhash)
		return nil
	} else {
		//rlog.Tracef(2, "TX %s rejected by pool by mempool", txhash)
		return fmt.Errorf("TX %s rejected by pool by mempool", txhash)
	}

}

// side blocks are blocks which lost the race the to become part
// of main chain,
// a block is a side block if it satisfies the following condition
// if no other block exists on this height before this
// this is part of consensus rule
// this is the topoheight of this block itself
func (chain *Blockchain) Isblock_SideBlock(blid crypto.Hash) bool {
	block_topoheight := chain.Load_Block_Topological_order(blid)
	if block_topoheight == 0 {
		return false
	}
	// lower reward for byzantine behaviour
	// for as many block as added
	block_height := chain.Load_Height_for_BL_ID(blid)

	return chain.isblock_SideBlock_internal(blid, block_topoheight, block_height)
}

// todo optimize/ run more checks
func (chain *Blockchain) isblock_SideBlock_internal(blid crypto.Hash, block_topoheight int64, block_height int64) (result bool) {
	if block_topoheight == 0 { // genesis cannot be side block
		return false
	}

	toporecord, err := chain.Store.Topo_store.Read(block_topoheight - 1)
	if err != nil {
		panic("Could not load block from previous order")
	}
	if block_height == toporecord.Height { // lost race (or byzantine behaviour)
		return true
	}
	return false
}

// this will return the tx combination as valid/invalid
// this is not used as core consensus but reports only to user that his tx though in the blockchain is invalid
// a tx is valid, if it exist in a block which is not a side block
func (chain *Blockchain) IS_TX_Valid(txhash crypto.Hash) (valid_blid crypto.Hash, invalid_blid []crypto.Hash, valid bool) {

	var tx_bytes []byte
	var err error

	if tx_bytes, err = chain.Store.Block_tx_store.ReadTX(txhash); err != nil {
		return
	}

	var tx transaction.Transaction
	if err = tx.Deserialize(tx_bytes); err != nil {
		return
	}

	var blids_list []crypto.Hash

	for i := uint64(1); i < 2*TX_VALIDITY_HEIGHT; i++ {
		blids, _ := chain.Store.Topo_store.binarySearchHeight(int64(tx.Height + i))
		blids_list = append(blids_list, blids...)
	}

	var exist_list []crypto.Hash

	for _, blid := range blids_list {
		bl, err := chain.Load_BL_FROM_ID(blid)
		if err != nil {
			return
		}

		for _, bltxhash := range bl.Tx_hashes {
			if bltxhash == txhash {
				exist_list = append(exist_list, blid)
				break
			}
		}
	}

	for _, blid := range exist_list {
		if chain.Isblock_SideBlock(blid) {
			invalid_blid = append(invalid_blid, blid)
		} else {
			valid_blid = blid
			valid = true
		}

	}

	return
}

/*


// runs the client protocol which includes the following operations
// if any TX are being duplicate or double-spend ignore them
// mark all the valid transactions as valid
// mark all invalid transactions  as invalid
// calculate total fees based on valid TX
// we need NOT check ranges/ring signatures here, as they have been done already by earlier steps
func (chain *Blockchain) client_protocol(dbtx storage.DBTX, bl *block.Block, blid crypto.Hash, height int64, topoheight int64) (total_fees uint64) {
	// run client protocol for all TXs
	for i := range bl.Tx_hashes {
		tx, err := chain.Load_TX_FROM_ID(dbtx, bl.Tx_hashes[i])
		if err != nil {
			panic(fmt.Errorf("Cannot load  tx for %x err %s ", bl.Tx_hashes[i], err))
		}
		// mark TX found in this block also  for explorer
		chain.store_TX_in_Block(dbtx, blid, bl.Tx_hashes[i])

		// check all key images as double spend, if double-spend detected mark invalid, else consider valid
		if chain.Verify_Transaction_NonCoinbase_DoubleSpend_Check(dbtx, tx) {

			chain.consume_keyimages(dbtx, tx, height) // mark key images as consumed
			total_fees += tx.RctSignature.Get_TX_Fee()

			chain.Store_TX_Height(dbtx, bl.Tx_hashes[i], topoheight) // link the tx with the topo height

			//mark tx found in this block is valid
			chain.mark_TX(dbtx, blid, bl.Tx_hashes[i], true)

		} else { // TX is double spend or reincluded by 2 blocks simultaneously
			rlog.Tracef(1,"Double spend TX is being ignored %s %s", blid, bl.Tx_hashes[i])
			chain.mark_TX(dbtx, blid, bl.Tx_hashes[i], false)
		}
	}

	return total_fees
}

// this undoes everything that is done by client protocol
// NOTE: this will have any effect, only if client protocol has been run on this block earlier
func (chain *Blockchain) client_protocol_reverse(dbtx storage.DBTX, bl *block.Block, blid crypto.Hash) {
	// run client protocol for all TXs
	for i := range bl.Tx_hashes {
		tx, err := chain.Load_TX_FROM_ID(dbtx, bl.Tx_hashes[i])
		if err != nil {
			panic(fmt.Errorf("Cannot load  tx for %x err %s ", bl.Tx_hashes[i], err))
		}
		// only the  valid TX must be revoked
		if chain.IS_TX_Valid(dbtx, blid, bl.Tx_hashes[i]) {
			chain.revoke_keyimages(dbtx, tx) // mark key images as not used

			chain.Store_TX_Height(dbtx, bl.Tx_hashes[i], -1) // unlink the tx with the topo height

			//mark tx found in this block is invalid
			chain.mark_TX(dbtx, blid, bl.Tx_hashes[i], false)

		} else { // TX is double spend or reincluded by 2 blocks simultaneously
			// invalid tx is related
		}
	}

	return
}

// scavanger for transactions from rusty/stale tips to reinsert them into pool
func (chain *Blockchain) transaction_scavenger(dbtx storage.DBTX, blid crypto.Hash) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warnf("Recovered while transaction scavenging, Stack trace below ")
			logger.Warnf("Stack trace  \n%s", debug.Stack())
		}
	}()

	logger.Debugf("scavenging transactions from blid %s", blid)
	reachable_blocks := chain.BuildReachableBlocks(dbtx, []crypto.Hash{blid})
	reachable_blocks[blid] = true // add self
	for k, _ := range reachable_blocks {
		if chain.Is_Block_Orphan(k) {
			bl, err := chain.Load_BL_FROM_ID(dbtx, k)
			if err == nil {
				for i := range bl.Tx_hashes {
					tx, err := chain.Load_TX_FROM_ID(dbtx, bl.Tx_hashes[i])
					if err != nil {
						rlog.Warnf("err while scavenging blid %s  txid %s err %s", k, bl.Tx_hashes[i], err)
					} else {
						// add tx to pool, it will do whatever is necessarry
						chain.Add_TX_To_Pool(tx)
					}
				}
			} else {
				rlog.Warnf("err while scavenging blid %s err %s", k, err)
			}
		}
	}
}

*/
// Finds whether a  block is orphan
// since we donot store any fields, we need to calculate/find the block as orphan
// using an algorithm
// if the block is NOT topo ordered , it is orphan/stale
func (chain *Blockchain) Is_Block_Orphan(hash crypto.Hash) bool {
	return !chain.Is_Block_Topological_order(hash)
}

// this is used to find if a tx is orphan, YES orphan TX
// these can occur during  when they lie only  in a side block
// so the TX becomes orphan ( chances are less may be less that .000001 % but they are there)
// if a tx is not valid in any of the blocks, it has been mined it is orphan
func (chain *Blockchain) Is_TX_Orphan(hash crypto.Hash) (result bool) {
	_, _, result = chain.IS_TX_Valid(hash)
	return !result
}

// verifies whether we are lagging
// return true if we need resync
// returns false if we are good and resync is not required
func (chain *Blockchain) IsLagging(peer_cdiff *big.Int) bool {

	our_diff := new(big.Int).SetInt64(0)

	high_block, err := chain.Load_Block_Topological_order_at_index(chain.Load_TOPO_HEIGHT())
	if err != nil {
		return false
	} else {
		our_diff = chain.Load_Block_Cumulative_Difficulty(high_block)
	}
	//fmt.Printf("P_cdiff %s cdiff %s  our top block %s", peer_cdiff.String(), our_diff.String(), high_block)

	if our_diff.Cmp(peer_cdiff) < 0 {
		return true // peer's cumulative difficulty is more than ours , active resync
	}
	return false
}

// this function will rewind the chain from the topo height one block at a time
// this function also runs the client protocol in reverse and also deletes the block from the storage
func (chain *Blockchain) Rewind_Chain(rewind_count int) (result bool) {
	defer chain.Initialise_Chain_From_DB()

	chain.Lock()
	defer chain.Unlock()

	// we must till we reach a safe point
	// safe point is point where a single block exists at specific height
	// this may lead us to rewinding a it more
	//safe := false

	// TODO we must fix safeness using the stable calculation

	if rewind_count == 0 {
		return
	}

	top_block_topo_index := chain.Load_TOPO_HEIGHT()
	rewinded := int64(0)

	for { // rewind as many as possible
		if top_block_topo_index-rewinded < 1 || rewinded >= int64(rewind_count) {
			break
		}

		rewinded++
	}

	for { // rewinf till we reach a safe point
		r, err := chain.Store.Topo_store.Read(top_block_topo_index - rewinded)
		if err != nil {
			panic(err)
		}

		if chain.IsBlockSyncBlockHeight(r.BLOCK_ID) || r.Height == 1 {
			break
		}

		rewinded++
	}

	for i := int64(0); i != rewinded; i++ {
		chain.Store.Topo_store.Clean(top_block_topo_index - i)
	}

	return true
}

// this is part of consensus rule, 2 tips cannot refer to different parents
func (chain *Blockchain) CheckDagStructure(tips []crypto.Hash) bool {
	if chain.Load_Height_for_BL_ID(tips[0]) <= 2 { //  before this we cannot complete checks
		return true
	}

	for i := range tips { // first make sure all the tips are at same height
		if chain.Load_Height_for_BL_ID(tips[0]) != chain.Load_Height_for_BL_ID(tips[i]) {

			return false
		}
	}

	switch len(tips) {
	case 1:
		past := chain.Get_Block_Past(tips[0])
		switch len(past) {
		case 1: // nothing to do here

		case 2:
			if chain.Load_Height_for_BL_ID(past[0]) != chain.Load_Height_for_BL_ID(past[1]) {
				return false
			}

			past0 := chain.Get_Block_Past(past[0])
			if len(past0) != 1 { //only 1 tip in past
				return false
			}
			past1 := chain.Get_Block_Past(past[1])
			if len(past1) != 1 { //only 1 tip in past
				fmt.Printf("checking tips %+v past1 failed %d for %s\n", tips, len(past0), tips[0])
				return false
			}

			if past0[0] != past1[0] { // avoid any tips which fail reachability test
				return false
			}

		}
	case 2: // lets make sure both tips originate from same parent
		pasttip0 := chain.Get_Block_Past(tips[0])
		if len(pasttip0) != 1 { //only 1 tip in past
			return false
		}
		pasttip1 := chain.Get_Block_Past(tips[1])
		if len(pasttip0) != len(pasttip1) {
			return false
		}
		if pasttip0[0] != pasttip1[0] { // avoid any tips which fail reachability test
			return false
		}

	default:
		return false

	}

	return true
}

// sync blocks have the following specific property
// 1) the block is singleton at this height
// basically the condition allow us to confirm weight of future blocks with reference to sync blocks
// these are the one who settle the chain and guarantee it
func (chain *Blockchain) IsBlockSyncBlockHeight(blid crypto.Hash) bool {
	return chain.IsBlockSyncBlockHeightSpecific(blid, chain.Get_Height())
}

func (chain *Blockchain) IsBlockSyncBlockHeightSpecific(blid crypto.Hash, chain_height int64) bool {

	// TODO make sure that block exist
	height := chain.Load_Height_for_BL_ID(blid)
	if height == 0 { // genesis is always a sync block
		return true
	}

	//  top blocks are always considered unstable
	if (height + config.STABLE_LIMIT) > chain_height {
		return false
	}

	// if block is not ordered, it can never be sync block
	if !chain.Is_Block_Topological_order(blid) {
		return false
	}

	blocks := chain.Get_Blocks_At_Height(height)

	if len(blocks) == 0 && height != 0 { // this  should NOT occur
		panic("No block exists at this height, not possible")
	}
	if len(blocks) != 1 { //  ideal blockchain case, it is a sync block
		return false
	}

	return true
}

// converts a DAG's partial order into a full order, this function is recursive
// dag can be processed only one height at a time
// blocks are ordered recursively, till we find a find a block  which is already in the chain
func (chain *Blockchain) Generate_Full_Order_New(current_tip crypto.Hash, new_tip crypto.Hash) (order []crypto.Hash, topo int64) {

	if chain.Load_Height_for_BL_ID(new_tip) != chain.Load_Height_for_BL_ID(current_tip)+1 {
		panic("dag can only grow one height at a time")
	}

	depth := 20
	for ; ; depth += 20 {
		current_history := chain.get_ordered_past(current_tip, depth)
		new_history := chain.get_ordered_past(new_tip, depth)

		if len(current_history) < 5 { // we assume chain will not fork before 4 blocks
			var current_history_rev []crypto.Hash
			var new_history_rev []crypto.Hash

			for i := range current_history {
				current_history_rev = append(current_history_rev, current_history[len(current_history)-i-1])
			}
			for i := range new_history {
				new_history_rev = append(new_history_rev, new_history[len(new_history)-i-1])
			}

			for j := range new_history_rev {
				found := false
				for i := range current_history_rev {
					if current_history_rev[i] == new_history_rev[j] {
						found = true
						break
					}
				}

				if !found { // we have a contention point
					topo = chain.Load_Block_Topological_order(new_history_rev[j-1]) + 1
					order = append(order, new_history_rev[j:]...) //  order is already stored and store
					return
				}
			}
			panic("not possible")
		}

		for i := 0; i < len(current_history)-4; i++ {
			for j := 0; j < len(new_history)-4; j++ {
				if current_history[i+0] == new_history[j+0] &&
					current_history[i+1] == new_history[j+1] &&
					current_history[i+2] == new_history[j+2] &&
					current_history[i+3] == new_history[j+3] {

					topo = chain.Load_Block_Topological_order(new_history[j])
					for k := j; k >= 0; k-- {
						order = append(order, new_history[k]) // reverse order and store
					}
					return

				}
			}
		}
	}

	return
}

// we will collect atleast 50 blocks  or till genesis
func (chain *Blockchain) get_ordered_past(tip crypto.Hash, count int) (order []crypto.Hash) {
	order = append(order, tip)
	current := tip
	for len(order) < count {
		past := chain.Get_Block_Past(current)

		switch len(past) {
		case 0: // we reached genesis return
			return

		case 1:
			order = append(order, past[0])
			current = past[0]
		case 2:
			if bytes.Compare(past[0][:], past[1][:]) < 0 {
				order = append(order, past[0], past[1])
			} else {
				order = append(order, past[1], past[0])
			}
			current = past[0]
		default:
			panic("data corruption")
		}
	}
	return
}