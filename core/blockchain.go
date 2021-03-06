package core

import (
	"tinychain/db"
	"github.com/hashicorp/golang-lru"
	"tinychain/core/types"
	"tinychain/consensus"
	"tinychain/common"
	"sync/atomic"
	"sync"
	"errors"
	"fmt"
	"tinychain/core/state"
)

var (
	log = common.GetLogger("blockchain")
)

const (
	blockCacheLimit  = 1024
	headerCacheLimit = 2048
)

// Blockchain is the canonical chain given a database with a genesis block
type Blockchain struct {
	db             *db.TinyDB       // chain db
	genesis        *types.Block     // genesis block
	lastBlock      atomic.Value     // last block of chain
	lastFinalBlock atomic.Value     // last final block of chian
	engine         consensus.Engine // consensus engine
	mu             sync.RWMutex

	blocksCache *lru.Cache // blocks lru cache
	headerCache *lru.Cache // headers lru cache
}

func NewBlockchain(db *db.TinyDB, engine consensus.Engine) (*Blockchain, error) {
	blocksCache, _ := lru.New(blockCacheLimit)
	headerCache, _ := lru.New(headerCacheLimit)
	bc := &Blockchain{
		db:          db,
		engine:      engine,
		blocksCache: blocksCache,
		headerCache: headerCache,
	}
	if err := bc.loadLastState(); err != nil {
		log.Errorf("failed to load last state from db, err:%s", err)
		return nil, err
	}
	bc.genesis = bc.GetBlockByHeight(0)

	return bc, nil
}

// loadLastState load the latest state of blockchain
func (bc *Blockchain) loadLastState() error {
	lastBlock := bc.LastBlock()
	if lastBlock != nil {
		// Should create genensis block
		return bc.Reset()
	}

	if _, err := state.New(bc.db.LDB(), lastBlock.StateRoot().Bytes()); err != nil {
		log.Errorf("failed to init state, err:%s", err)
		return err
	}

	bc.blocksCache.Add(lastBlock.Height(), lastBlock)
	// TODO

	return nil
}

// Reset init blockchain with genesis block
func (bc *Blockchain) Reset() error {
	return bc.ResetWithGenesis(bc.genesis)
}

func (bc *Blockchain) ResetWithGenesis(genesis *types.Block) error {
	bc.clear()

	if _, err := state.New(bc.db.LDB(), genesis.StateRoot().Bytes()); err != nil {
		log.Errorf("failed to reset blockchain with genesis, err:%s", err)
		return err
	}

	if err := bc.db.PutBlock(bc.db.LDB().NewBatch(), genesis, false, true); err != nil {
		log.Errorf("failed to put genesis into db, err:%s", err)
		return err
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if err := bc.db.PutLastBlock(genesis.Hash()); err != nil {
		log.Errorf("failed to put genesis hash into db, err:%s", err)
		return err
	}
	bc.blocksCache.Add(genesis.Height(), genesis)
	bc.genesis = genesis
	bc.lastBlock.Store(genesis)

	return nil
}

func (bc *Blockchain) clear() {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.lastBlock.Store(nil)
	bc.blocksCache.Purge()
	bc.headerCache.Purge()
}

// LastBlock returns the last block of latest blockchain in memory
func (bc *Blockchain) LastBlock() *types.Block {
	if block := bc.lastBlock.Load(); block != nil {
		return block.(*types.Block)
	}
	block := bc.LastFinalBlock()
	bc.lastBlock.Store(block)
	return block
}

// LastFinalBlock returns the last commited block in db
func (bc *Blockchain) LastFinalBlock() *types.Block {
	if fb := bc.lastFinalBlock.Load(); fb != nil {
		return fb.(*types.Block)
	}
	hash, err := bc.db.GetLastBlock()
	if err != nil {
		panic(fmt.Sprintf("failed to get last block's hash from db, err:%s", err))
	}
	block := bc.GetBlockByHash(hash)
	bc.lastFinalBlock.Store(block)
	return block
}

func (bc *Blockchain) GetBlock(hash common.Hash, height uint64) *types.Block {
	block, err := bc.db.GetBlock(height, hash)
	if err != nil {
		log.Errorf("failed to get block from db, err:%s", err)
		return nil
	}
	bc.blocksCache.Add(hash, block)
	return block
}

func (bc *Blockchain) GetBlockByHeight(height uint64) *types.Block {
	hash, err := bc.db.GetHash(height)
	if err != nil {
		log.Errorf("failed to get hash from db, err:%s", err)
		return nil
	}
	return bc.GetBlock(hash, height)
}

func (bc *Blockchain) GetBlockByHash(hash common.Hash) *types.Block {
	if block, ok := bc.blocksCache.Get(hash); ok {
		return block.(*types.Block)
	}
	height, err := bc.db.GetHeight(hash)
	if err != nil {
		log.Errorf("failed to get height by hash from db, err:%s", err)
		return nil
	}
	return bc.GetBlock(hash, height)
}

func (bc *Blockchain) GetHeaderByHash(hash common.Hash) *types.Header {
	height, err := bc.db.GetHeight(hash)
	if err != nil {
		return nil
	}
	if header, ok := bc.headerCache.Get(height); ok {
		return header.(*types.Header)
	}
	header, err := bc.db.GetHeader(height, hash)
	if err != nil {
		return nil
	}
	bc.headerCache.Add(hash, header)
	return header
}

// AddBlocks insert blocks in batch when importing outer blockchain
func (bc *Blockchain) AddBlocks(blocks types.Blocks) error {
	for _, block := range blocks {
		if err := bc.AddBlock(block); err != nil {
			log.Errorf("failed to add block %s, err:%s", block.Hash(), err)
			return err
		}
	}

	return nil
}

// AddBlock appends block into chain.
// The blocks passed have been validated by block_pool.
func (bc *Blockchain) AddBlock(block *types.Block) error {
	if blk := bc.GetBlockByHash(block.Hash()); blk != nil {
		return errors.New(fmt.Sprintf("block %s exists in blockchain", blk.Hash().Hex()))
	}
	last := bc.LastBlock()
	// Check block height equals to last height+1 or not
	if block.Height() != last.Height()+1 {
		return errors.New(fmt.Sprintf(
			"block #%d cannot be added into blockchain because its previous block height is #%d",
			block.Height(), last.Height()))
	}
	bc.blocksCache.Add(block.Hash(), block)
	bc.lastBlock.Store(block)
	return nil
}

// commit persist the block to db.
func (bc *Blockchain) CommitBlock(block *types.Block) error {
	// Put block to db.Batch
	bc.db.PutBlock(db.GetBatch(bc.db.LDB(), block.Height()), block, false, false)
	if err := bc.db.PutLastBlock(block.Hash()); err != nil {
		log.Errorf("failed to put last block hash to db, err:%s", err)
		return err
	}
	bc.lastFinalBlock.Store(block)
	return nil
}

func (bc *Blockchain) Engine() consensus.Engine {
	return bc.engine
}
