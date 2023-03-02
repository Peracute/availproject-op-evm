package watchtower

import (
	"crypto/ecdsa"
	"errors"
	"fmt"

	"github.com/0xPolygon/polygon-edge/blockchain"
	"github.com/0xPolygon/polygon-edge/crypto"
	"github.com/0xPolygon/polygon-edge/state"
	"github.com/0xPolygon/polygon-edge/txpool"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
	"github.com/maticnetwork/avail-settlement/pkg/block"
	"github.com/maticnetwork/avail-settlement/pkg/staking"
)

var (
	// ErrInvalidBlock is general error used when block structure is invalid
	// or its field values are inconsistent.
	ErrInvalidBlock = errors.New("invalid block")

	// ErrParentBlockNotFound is returned when the local blockchain doesn't
	// contain block for the referenced parent hash.
	ErrParentBlockNotFound = errors.New("parent block not found")

	// FraudproofPrefix is byte sequence that prefixes the fraudproof objected
	// malicious block hash in `ExtraData` of the fraudproof block header.
	FraudproofPrefix = []byte("FRAUDPROOF_OF:")

	// NoBlockValidation is here to help us if we do not need to pass extra block validation
	NoBlockValidation = func(_ *types.Block) (error, bool) { return nil, false }
)

type WatchTower interface {
	Apply(blk *types.Block) error
	CheckBlockFully(blk *types.Block) error
	ConstructFraudproof(blk *types.Block) (*types.Block, error)
}

type BlockValidationFn = func(blk *types.Block) (error, bool)

type watchTower struct {
	blockchain          *blockchain.Blockchain
	executor            *state.Executor
	txpool              *txpool.TxPool
	blockBuilderFactory block.BlockBuilderFactory
	logger              hclog.Logger
	validationFn        BlockValidationFn

	account types.Address
	signKey *ecdsa.PrivateKey
}

func New(blockchain *blockchain.Blockchain, executor *state.Executor, txp *txpool.TxPool, fn BlockValidationFn, logger hclog.Logger, account types.Address, signKey *ecdsa.PrivateKey) WatchTower {
	return &watchTower{
		blockchain:          blockchain,
		executor:            executor,
		txpool:              txp,
		validationFn:        fn,
		logger:              logger,
		blockBuilderFactory: block.NewBlockBuilderFactory(blockchain, executor, hclog.Default()),

		account: account,
		signKey: signKey,
	}
}

func (wt *watchTower) CheckBlockFully(blk *types.Block) error {
	if blk == nil {
		return fmt.Errorf("%w: block == nil", ErrInvalidBlock)
	}

	if blk.Header == nil {
		return fmt.Errorf("%w: block.Header == nil", ErrInvalidBlock)
	}

	// No matter error, we should return that it's safe as in case of any errors, we're wont write
	// block into sequencers and watchtower should not be doing a fraud check on those.
	if err, _ := wt.validationFn(blk); err != nil {
		wt.logger.Warn(
			"block cannot be verified and it's not necessary to build fraud proof",
			"block_number", blk.Number(),
			"block_hash", blk.Hash(),
			"parent_block_hash", blk.ParentHash(),
			"error", err,
		)
		return nil
	}

	return nil
}

func (wt *watchTower) Apply(blk *types.Block) error {
	if err := wt.blockchain.WriteBlock(blk, block.SourceWatchTower); err != nil {
		return fmt.Errorf("failed to write block: %w", err)
	}

	// after the block has been written we reset the txpool so that
	// the old transactions are removed
	wt.txpool.ResetWithHeaders(blk.Header)

	wt.logger.Info("Block committed to blockchain", "block_number", blk.Header.Number, "hash", blk.Header.Hash.String())
	wt.logger.Debug("Received block header", "block_header", blk.Header)
	wt.logger.Debug("Received block transactions", "block_transactions", blk.Transactions)

	return nil
}

func (wt *watchTower) ConstructFraudproof(maliciousBlock *types.Block) (*types.Block, error) {
	builder, err := wt.blockBuilderFactory.FromParentHash(maliciousBlock.ParentHash())
	if err != nil {
		return nil, err
	}

	fraudProofTxs, err := constructFraudproofTxs(wt.account, maliciousBlock)
	if err != nil {
		return nil, err
	}

	hdr, _ := wt.blockchain.GetHeaderByHash(maliciousBlock.ParentHash())
	transition, err := wt.executor.BeginTxn(hdr.StateRoot, hdr, wt.account)
	if err != nil {
		return nil, err
	}

	txSigner := &crypto.FrontierSigner{}
	fpTx := fraudProofTxs[0]
	fpTx.Nonce = transition.GetNonce(fpTx.From)
	tx, err := txSigner.SignTx(fpTx, wt.signKey)
	if err != nil {
		return nil, err
	}

	if wt.txpool != nil { // Tests sometimes do not have txpool so we need to do this check.
		if err := wt.txpool.AddTx(tx); err != nil {
			wt.logger.Error("failed to add fraud proof txn to the pool", "error", err)
			return nil, err
		}
	}

	wt.logger.Info(
		"Applied dispute resolution transaction to the txpool",
		"hash", tx.Hash,
		"nonce", tx.Nonce,
		"account_from", tx.From,
	)

	// Build the block that is going to be sent out to the Avail.
	blk, err := builder.
		SetCoinbaseAddress(wt.account).
		SetGasLimit(maliciousBlock.Header.GasLimit).
		SetExtraDataField(block.KeyFraudProofOf, maliciousBlock.Hash().Bytes()).
		SetExtraDataField(block.KeyBeginDisputeResolutionOf, tx.Hash.Bytes()).
		AddTransactions(fraudProofTxs...).
		SignWith(wt.signKey).
		Build()

	if err != nil {
		return nil, err
	}

	return blk, nil
}

// constructFraudproofTxs returns set of transactions that challenge the
// malicious block and submit watchtower's stake.
func constructFraudproofTxs(watchtowerAddress types.Address, maliciousBlock *types.Block) ([]*types.Transaction, error) {
	bdrTx, err := constructBeginDisputeResolutionTx(watchtowerAddress, maliciousBlock)
	if err != nil {
		return []*types.Transaction{}, err
	}

	return []*types.Transaction{bdrTx}, nil
}

func constructBeginDisputeResolutionTx(watchtowerAddress types.Address, maliciousBlock *types.Block) (*types.Transaction, error) {
	tx, err := staking.BeginDisputeResolutionTx(watchtowerAddress, types.BytesToAddress(maliciousBlock.Header.Miner), maliciousBlock.Header.GasLimit)
	if err != nil {
		return nil, err
	}

	return tx, nil
}
