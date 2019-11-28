package bitcoincash

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	btchd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/cpacia/multiwallet/base"
	"github.com/cpacia/multiwallet/client/bchd"
	"github.com/cpacia/multiwallet/database"
	iwallet "github.com/cpacia/wallet-interface"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/blockchain"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil/hdkeychain"
	"github.com/gcash/bchutil/txsort"
	"github.com/gcash/bchwallet/wallet/txauthor"
	"github.com/gcash/bchwallet/wallet/txrules"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchutil"
	"time"
)

// Assert interfaces
var _ = iwallet.Wallet(&BitcoinCashWallet{})
var _ = iwallet.WalletCrypter(&BitcoinCashWallet{})
var _ = iwallet.Escrow(&BitcoinCashWallet{})
var _ = iwallet.EscrowWithTimeout(&BitcoinCashWallet{})

var feeLevels = map[iwallet.FeeLevel]iwallet.Amount{
	iwallet.FlEconomic: iwallet.NewAmount(1),
	iwallet.FlNormal:   iwallet.NewAmount(5),
	iwallet.FlPriority: iwallet.NewAmount(10),
}

// BitcoinCashWallet extends wallet base and implements the
// remaining functions for each interface.
type BitcoinCashWallet struct {
	base.WalletBase
	testnet bool
}

// NewBitcoinCashWallet returns a new BitcoinCashWallet. This constructor
// attempts to connect to the API. If it fails, it will not build.
func NewBitcoinCashWallet(cfg *base.WalletConfig) (*BitcoinCashWallet, error) {
	w := &BitcoinCashWallet{
		testnet: cfg.Testnet,
	}

	chainClient, err := bchd.NewBchdClient(cfg.ClientUrl)
	if err != nil {
		return nil, err
	}

	w.ChainClient = chainClient
	w.DataDir = cfg.DataDir
	w.Logger = cfg.Logger
	w.CoinType = iwallet.CtBitcoinCash
	w.AddressFunc = func(key *btchd.ExtendedKey) (iwallet.Address, error) {
		newKey, err := hdkeychain.NewKeyFromString(key.String())
		if err != nil {
			return iwallet.Address{}, err
		}
		addr, err := newKey.Address(w.params())
		if err != nil {
			return iwallet.Address{}, err
		}
		return iwallet.NewAddress(addr.String(), iwallet.CtBitcoinCash), nil
	}
	return w, nil
}

// ValidateAddress validates that the serialization of the address is correct
// for this coin and network. It returns an error if it isn't.
func (w *BitcoinCashWallet) ValidateAddress(addr iwallet.Address) error {
	_, err := bchutil.DecodeAddress(addr.String(), w.params())
	return err
}

// IsDust returns whether the amount passed in is considered dust by network. This
// method is called when building payout transactions from the multisig to the various
// participants. If the amount that is supposed to be sent to a given party is below
// the dust threshold, openbazaar-go will not pay that party to avoid building a transaction
// that never confirms.
func (w *BitcoinCashWallet) IsDust(amount iwallet.Amount) bool {
	return txrules.IsDustAmount(bchutil.Amount(amount.Int64()), 25, txrules.DefaultRelayFeePerKb)
}

// EstimateSpendFee should return the anticipated fee to transfer a given amount of coins
// out of the wallet at the provided fee level. Typically this involves building a
// transaction with enough inputs to cover the request amount and calculating the size
// of the transaction. It is OK, if a transaction comes in after this function is called
// that changes the estimated fee as it's only intended to be an estimate.
//
// All amounts should be in the coin's base unit (for example: satoshis).
func (w *BitcoinCashWallet) EstimateSpendFee(amount iwallet.Amount, feeLevel iwallet.FeeLevel) (iwallet.Amount, error) {
	// Since this is an estimate we can use a dummy output address. Let's use a long one so we don't under estimate.
	tx, err := w.buildTx(amount.Int64(), iwallet.NewAddress("qpf464w2g36kyklq9shvyjk9lvuf6ph7jv3k8qpq0m", iwallet.CtBitcoinCash), feeLevel)
	if err != nil {
		return iwallet.NewAmount(0), err
	}
	var outval int64
	for _, output := range tx.TxOut {
		outval += output.Value
	}
	var utxoRecords []database.UtxoRecord
	err = w.DB.View(func(dbtx database.Tx) error {
		return dbtx.Read().Where("coin = ?", w.CoinType.CurrencyCode()).Find(&utxoRecords).Error
	})
	if err != nil {
		return iwallet.NewAmount(0), err
	}

	var inval int64
	for _, input := range tx.TxIn {
		for _, utxo := range utxoRecords {
			var op wire.OutPoint
			ser, err := hex.DecodeString(utxo.Outpoint)
			if err != nil {
				return iwallet.NewAmount(0), err
			}
			if err := op.Deserialize(bytes.NewReader(ser)); err != nil {
				return iwallet.NewAmount(0), err
			}

			if op.Hash.IsEqual(&input.PreviousOutPoint.Hash) && op.Index == input.PreviousOutPoint.Index {
				inval += iwallet.NewAmount(utxo.Amount).Int64()
				break
			}
		}
	}
	if inval < outval {
		return iwallet.NewAmount(0), errors.New("error building transaction: inputs less than outputs")
	}
	return iwallet.NewAmount(inval - outval), err
}

// Spend is a request to send requested amount to the requested address. The
// fee level is provided by the user. It's up to the implementation to decide
// how best to use the fee level.
//
// The database Tx MUST be respected. When this function is called the wallet
// state changes should be prepped and held in memory. If Rollback() is called
// the state changes should be discarded. Only when Commit() is called should
// the state changes be applied and the transaction broadcasted to the network.
func (w *BitcoinCashWallet) Spend(dbtx iwallet.Tx, to iwallet.Address, amt iwallet.Amount, feeLevel iwallet.FeeLevel) (iwallet.TransactionID, error) {
	tx, err := w.buildTx(amt.Int64(), to, feeLevel)
	if err != nil {
		return iwallet.TransactionID(""), err
	}
	var buf bytes.Buffer
	if err := tx.BchEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		return iwallet.TransactionID(""), err
	}

	wtx, ok := dbtx.(*base.DBTx)
	if !ok {
		return iwallet.TransactionID(""), errors.New("error type asserting database tx")
	}

	itx, err := w.txToInterfaceFormat(tx)
	if err != nil {
		return iwallet.TransactionID(""), err
	}

	wtx.OnCommit = func() error {
		err := w.ChainClient.Broadcast(buf.Bytes())
		if err != nil {
			return err
		}
		return w.ChainManager.IngestTransaction(itx)
	}
	return iwallet.TransactionID(tx.TxHash().String()), nil
}

// SweepWallet should sweep the full balance of the wallet to the requested
// address. It is expected for most coins that the fee will be subtracted
// from the amount sent rather than added to it.
func (w *BitcoinCashWallet) SweepWallet(dbtx iwallet.Tx, to iwallet.Address, level iwallet.FeeLevel) (iwallet.TransactionID, error) {
	return iwallet.TransactionID(""), nil
}

// EstimateEscrowFee estimates the fee to release the funds from escrow.
// this assumes only one input. If there are more inputs OpenBazaar will
// will add 50% of the returned fee for each additional input. This is a
// crude fee calculating but it simplifies things quite a bit.
func (w *BitcoinCashWallet) EstimateEscrowFee(threshold int, level iwallet.FeeLevel) (iwallet.Amount, error) {
	return iwallet.NewAmount(0), nil
}

// CreateMultisigAddress creates a new threshold multisig address using the
// provided pubkeys and the threshold. The multisig address is returned along
// with a byte slice. The byte slice will typically be the redeem script for
// the address (in Bitcoin related coins). The slice will be saved in OpenBazaar
// with the order and passed back into the wallet when signing the transaction.
// In practice this does not need to be a redeem script so long as the wallet
// knows how to sign the transaction when it sees it.
//
// This function should be deterministic as both buyer and vendor will be passing
// in the same set of keys and expecting to get back the same address and redeem
// script. If this is not the case the vendor will reject the order.
//
// Note that this is normally a 2 of 3 escrow in the normal case, however OpenBazaar
// also uses 1 of 2 multisigs as a form of a "cancelable" address when sending to
// a node that is offline. This allows the sender to cancel the payment if the vendor
// never comes back online.
func (w *BitcoinCashWallet) CreateMultisigAddress(keys []btcec.PublicKey, threshold int) (iwallet.Address, []byte, error) {
	if len(keys) < threshold {
		return iwallet.Address{}, nil, fmt.Errorf("unable to generate multisig script with "+
			"%d required signatures when there are only %d public "+
			"keys available", threshold, len(keys))
	}

	builder := txscript.NewScriptBuilder()
	builder.AddInt64(int64(threshold))
	for _, key := range keys {
		builder.AddData(key.SerializeCompressed())
	}
	builder.AddInt64(int64(len(keys)))
	builder.AddOp(txscript.OP_CHECKMULTISIG)

	redeemScript, err := builder.Script()
	if err != nil {
		return iwallet.Address{}, nil, err
	}
	addr, err := bchutil.NewAddressScriptHash(redeemScript, w.params())
	if err != nil {
		return iwallet.Address{}, nil, err
	}
	return iwallet.NewAddress(addr.String(), iwallet.CtBitcoinCash), redeemScript, nil
}

// SignMultisigTransaction should use the provided key to create a signature for
// the multisig transaction. Since this a threshold signature this function will
// separately by each party signing this transaction. The resulting signatures
// will be shared between the relevant parties and one of them will aggregate
// the signatures into a transaction for broadcast.
//
// For coins like bitcoin you may need to return one signature *per input* which is
// why a slice of signatures is returned.
func (w *BitcoinCashWallet) SignMultisigTransaction(txn iwallet.Transaction, key btcec.PrivateKey, redeemScript []byte) ([]iwallet.EscrowSignature, error) {
	return nil, nil
}

// BuildAndSend should used the passed in signatures to build the transaction.
// Note the signatures are a slice of slices. This is because coins like Bitcoin
// may require one signature *per input*. In this case the outer slice is the
// signatures from the different key holders and the inner slice is the keys
// per input.
// (TransactionID,
// Note a database transaction is used here. Same rules of Commit() and
// Rollback() apply.
func (w *BitcoinCashWallet) BuildAndSend(dbtx iwallet.Tx, txn iwallet.Transaction, signatures [][]iwallet.EscrowSignature, redeemScript []byte) (iwallet.TransactionID, error) {
	return iwallet.TransactionID(""), nil
}

// CreateMultisigWithTimeout is the same as CreateMultisigAddress but it adds
// an additional timeout to the address. The address should have two ways to
// release the funds:
//  - m of n signatures are provided (or)
//  - timeout has passed and a signature for timeoutKey is provided.
func (w *BitcoinCashWallet) CreateMultisigWithTimeout(keys []btcec.PublicKey, threshold int, timeout time.Duration, timeoutKey btcec.PublicKey) (iwallet.Address, []byte, error) {
	if len(keys) < threshold {
		return iwallet.Address{}, nil, fmt.Errorf("unable to generate multisig script with "+
			"%d required signatures when there are only %d public "+
			"keys available", threshold, len(keys))
	}

	builder := txscript.NewScriptBuilder()
	sequenceLock := blockchain.LockTimeToSequence(false, uint32(timeout.Hours()*6))
	builder.AddOp(txscript.OP_IF)
	builder.AddInt64(int64(threshold))
	for _, key := range keys {
		builder.AddData(key.SerializeCompressed())
	}
	builder.AddInt64(int64(len(keys)))
	builder.AddOp(txscript.OP_CHECKMULTISIG)
	builder.AddOp(txscript.OP_ELSE).
		AddInt64(int64(sequenceLock)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(timeoutKey.SerializeCompressed()).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ENDIF)

	redeemScript, err := builder.Script()
	if err != nil {
		return iwallet.Address{}, nil, err
	}
	addr, err := bchutil.NewAddressScriptHash(redeemScript, w.params())
	if err != nil {
		return iwallet.Address{}, nil, err
	}
	return iwallet.NewAddress(addr.String(), iwallet.CtBitcoinCash), redeemScript, nil
}

// ReleaseFundsAfterTimeout will release funds from the escrow. The signature will
// be created using the timeoutKey.
func (w *BitcoinCashWallet) ReleaseFundsAfterTimeout(dbtx iwallet.Tx, txn iwallet.Transaction, timeoutKey btcec.PrivateKey, redeemScript []byte) (iwallet.TransactionID, error) {
	return iwallet.TransactionID(""), nil
}

func (w *BitcoinCashWallet) params() *chaincfg.Params {
	if w.testnet {
		return &chaincfg.TestNet3Params
	} else {
		return &chaincfg.MainNetParams
	}
}

func (w *BitcoinCashWallet) feePerByte(level iwallet.FeeLevel) iwallet.Amount {
	return feeLevels[level]
}

func (w *BitcoinCashWallet) buildTx(amount int64, iaddr iwallet.Address, feeLevel iwallet.FeeLevel) (*wire.MsgTx, error) {
	// Check for dust
	addr, err := bchutil.DecodeAddress(iaddr.String(), w.params())
	if err != nil {
		return nil, err
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}
	if txrules.IsDustAmount(bchutil.Amount(amount), len(script), txrules.DefaultRelayFeePerKb) {
		return nil, errors.New("dust output amount")
	}

	var additionalPrevScripts map[wire.OutPoint][]byte
	var additionalKeysByAddress map[string]*bchutil.WIF
	var inVals map[wire.OutPoint]int64

	// Create input source
	coinKeyMap, err := w.GatherCoins()
	if err != nil {
		return nil, err
	}

	allCoins := make([]coinset.Coin, 0, len(coinKeyMap))
	for coin := range coinKeyMap {
		allCoins = append(allCoins, coin)
	}
	inputSource := func(target bchutil.Amount) (total bchutil.Amount, inputs []*wire.TxIn, inputValues []bchutil.Amount, scripts [][]byte, err error) {
		coinSelector := coinset.MaxValueAgeCoinSelector{MaxInputs: 10000, MinChangeAmount: btcutil.Amount(0)}
		coins, err := coinSelector.CoinSelect(btcutil.Amount(target.ToUnit(bchutil.AmountSatoshi)), allCoins)
		if err != nil {
			err = errors.New("insufficient funds")
			return
		}
		var (
			additionalPrevScripts   = make(map[wire.OutPoint][]byte)
			additionalKeysByAddress = make(map[string]*bchutil.WIF)
			inVals                  = make(map[wire.OutPoint]int64)
		)
		for _, c := range coins.Coins() {
			total += bchutil.Amount(c.Value().ToUnit(btcutil.AmountSatoshi))

			h, herr := chainhash.NewHashFromStr(c.Hash().String())
			if herr != nil {
				err = herr
				return
			}

			outpoint := wire.NewOutPoint(h, c.Index())

			in := wire.NewTxIn(outpoint, nil)
			inputs = append(inputs, in)

			additionalPrevScripts[*outpoint] = c.PkScript()

			key := coinKeyMap[c]
			hdKey, kerr := hdkeychain.NewKeyFromString(key.String())
			if kerr != nil {
				err = kerr
				return
			}
			privKey, perr := hdKey.ECPrivKey()
			if perr != nil {
				err = perr
				return
			}
			wif, werr := bchutil.NewWIF(privKey, w.params(), true)
			if werr != nil {
				err = werr
				return
			}

			additionalKeysByAddress[string(c.PkScript())] = wif

			sat := c.Value().ToUnit(btcutil.AmountSatoshi)
			inVals[*outpoint] = int64(sat)
		}
		return total, inputs, inputValues, scripts, nil
	}

	// Get the fee per kilobyte
	feePerKB := w.feePerByte(feeLevel).Int64() * 1000

	// outputs
	out := wire.NewTxOut(amount, script)

	// Create change source
	changeSource := func() ([]byte, error) {
		iaddr, err := w.Keychain.CurrentAddress(true)
		if err != nil {
			return nil, err
		}

		addr, err := bchutil.DecodeAddress(iaddr.String(), w.params())
		if err != nil {
			return nil, err
		}

		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
		return script, nil
	}

	// Build transaction
	authoredTx, err := txauthor.NewUnsignedTransaction([]*wire.TxOut{out}, bchutil.Amount(feePerKB), inputSource, changeSource)
	if err != nil {
		return nil, err
	}

	// BIP 69 sorting
	txsort.InPlaceSort(authoredTx.Tx)

	// Sign tx
	getKey := txscript.KeyClosure(func(addr bchutil.Address) (*bchec.PrivateKey, bool, error) {
		addrStr := addr.EncodeAddress()
		wif := additionalKeysByAddress[addrStr]
		return wif.PrivKey, wif.CompressPubKey, nil
	})

	getScript := txscript.ScriptClosure(func(
		addr bchutil.Address) ([]byte, error) {
		return nil, nil
	})

	for i, txIn := range authoredTx.Tx.TxIn {
		prevOutScript := additionalPrevScripts[txIn.PreviousOutPoint]

		script, err := txscript.SignTxOutput(w.params(),
			authoredTx.Tx, i, inVals[txIn.PreviousOutPoint], prevOutScript,
			txscript.SigHashAll, getKey, getScript, txIn.SignatureScript)
		if err != nil {
			return nil, errors.New("failed to sign transaction")
		}
		txIn.SignatureScript = script
	}
	return authoredTx.Tx, nil
}

func (w *BitcoinCashWallet) txToInterfaceFormat(tx *wire.MsgTx) (iwallet.Transaction, error) {
	txid := tx.TxHash()

	var utxoRecords []database.UtxoRecord
	err := w.DB.View(func(dbtx database.Tx) error {
		return dbtx.Read().Where("coin = ?", w.CoinType.CurrencyCode()).Find(&utxoRecords).Error
	})
	if err != nil {
		return iwallet.Transaction{}, err
	}

	itx := iwallet.Transaction{ID: iwallet.TransactionID(tx.TxHash().String())}
	for _, input := range tx.TxIn {
		for _, utxo := range utxoRecords {
			var op wire.OutPoint
			ser, err := hex.DecodeString(utxo.Outpoint)
			if err != nil {
				return itx, err
			}
			if err := op.Deserialize(bytes.NewReader(ser)); err != nil {
				return itx, err
			}

			if op.Hash.IsEqual(&input.PreviousOutPoint.Hash) && op.Index == input.PreviousOutPoint.Index {
				from := iwallet.SpendInfo{
					ID:      ser,
					Address: iwallet.NewAddress(utxo.Address, iwallet.CtBitcoinCash),
					Amount:  iwallet.NewAmount(utxo.Amount),
				}
				itx.From = append(itx.From, from)
				break
			}
		}
	}
	for i, out := range tx.TxOut {
		op := wire.NewOutPoint(&txid, uint32(i))
		var buf bytes.Buffer
		if err := op.Serialize(&buf); err != nil {
			return itx, err
		}

		_, addrs, _, err := txscript.ExtractPkScriptAddrs(out.PkScript, w.params())
		if err != nil {
			return itx, err
		}

		to := iwallet.SpendInfo{
			ID:      buf.Bytes(),
			Address: iwallet.NewAddress(addrs[0].String(), iwallet.CtBitcoinCash),
			Amount:  iwallet.NewAmount(out.Value),
		}
		itx.To = append(itx.To, to)
	}

	return itx, nil
}
