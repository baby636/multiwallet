package zcash

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/cpacia/multiwallet/base"
	"github.com/cpacia/multiwallet/database"
	"github.com/cpacia/multiwallet/database/sqlitedb"
	iwallet "github.com/cpacia/wallet-interface"
	"github.com/jarcoal/httpmock"
	"github.com/martinboehm/btcutil"
	"github.com/martinboehm/btcutil/txscript"
	"github.com/op/go-logging"
	"testing"
	"time"
)

func newTestWallet() (*ZCashWallet, error) {
	w := &ZCashWallet{
		testnet: true,
	}

	chainClient := base.NewMockChainClient()

	db, err := sqlitedb.NewMemoryDB()
	if err != nil {
		return nil, err
	}
	if err := database.InitializeDatabase(db); err != nil {
		return nil, err
	}

	w.ChainClient = chainClient
	w.DB = db
	w.Logger = logging.MustGetLogger("bchtest")
	w.CoinType = iwallet.CtZCash
	w.Done = make(chan struct{})
	w.AddressFunc = w.keyToAddress

	key, err := hdkeychain.NewKeyFromString("tprv8ZgxMBicQKsPeghT19pungdFLMJM2hMs3EEn5WtgobD7wuQSFQu4VNaEJXH9HS3RhhLT4wgZ3hj31m3kafuxhL9vfGTRtBVLSog4zjxW3L1")
	if err != nil {
		return nil, err
	}

	if err := w.CreateWallet(*key, nil, time.Now()); err != nil {
		return nil, err
	}

	if err := w.OpenWallet(); err != nil {
		return nil, err
	}
	return w, nil
}

func TestZCashWallet_ValidateAddress(t *testing.T) {
	tests := []struct {
		address iwallet.Address
		valid   bool
	}{
		{
			address: iwallet.NewAddress("abc", iwallet.CtZCash),
			valid:   false,
		},
		{
			address: iwallet.NewAddress("tmJKrg3gS4sPS7gSJ4vT8dFeqkGtfnDW4gu", iwallet.CtZCash),
			valid:   true,
		},
	}
	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}
	for i, test := range tests {
		err := w.ValidateAddress(test.address)
		if !test.valid && err == nil {
			t.Errorf("Test %d expected invalid address got valid", i)
		}
		if test.valid && err != nil {
			fmt.Println(err)
			t.Errorf("Test %d expected valid address got invalid", i)
		}
	}
}

func TestZCashWallet_IsDust(t *testing.T) {
	tests := []struct {
		amount iwallet.Amount
		isDust bool
	}{
		{
			amount: iwallet.NewAmount(0),
			isDust: true,
		},
		{
			amount: iwallet.NewAmount(545),
			isDust: true,
		},
		{
			amount: iwallet.NewAmount(546),
			isDust: false,
		},
	}
	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}
	for i, test := range tests {
		isDust := w.IsDust(test.amount)
		if test.isDust != isDust {
			t.Errorf("Test %d expected %t got %t", i, test.isDust, isDust)
		}
	}
}

func TestZCashWallet_EstimateSpendFee(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	tests := []struct {
		feeLevel      iwallet.FeeLevel
		amount        iwallet.Amount
		expected      iwallet.Amount
		expectedError error
	}{
		{
			amount:   iwallet.NewAmount(500000),
			feeLevel: iwallet.FlEconomic,
			expected: iwallet.NewAmount(375),
		},
		{
			amount:   iwallet.NewAmount(500000),
			feeLevel: iwallet.FlNormal,
			expected: iwallet.NewAmount(750),
		},
		{
			amount:   iwallet.NewAmount(500000),
			feeLevel: iwallet.FlPriority,
			expected: iwallet.NewAmount(1500),
		},
		{
			amount:        iwallet.NewAmount(1000000),
			feeLevel:      iwallet.FlPriority,
			expected:      iwallet.NewAmount(0),
			expectedError: base.ErrInsufficientFunds,
		},
	}

	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	b := make([]byte, 32)
	rand.Read(b)

	h, err := chainhash.NewHash(b)
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtZCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(serializeOutpoint(op)),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	for i, test := range tests {
		fee, err := w.EstimateSpendFee(test.amount, test.feeLevel)
		if err != test.expectedError {
			t.Errorf("Test %d: error: %s", i, err)
			continue
		}
		if fee.Cmp(test.expected) != 0 {
			t.Errorf("Test %d: expected %s, got %s", i, test.expected, fee)
		}
	}
}

func TestZCashWallet_Spend(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtZCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(serializeOutpoint(op)),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	wtx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w.Spend(wtx, iwallet.NewAddress("tmJKrg3gS4sPS7gSJ4vT8dFeqkGtfnDW4gu", iwallet.CtZCash), iwallet.NewAmount(500000), iwallet.FlNormal)
	if err != nil {
		t.Fatal(err)
	}

	expected := "c61df527468c4362d70842f0846fc04d66bc5e47d573f8ab3738f4706c025d6f"
	if txid.String() != expected {
		t.Errorf("Expected txid %s, got %s", expected, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtZCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestZCashWallet_SweepWallet(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtZCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(serializeOutpoint(op)),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	wtx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w.SweepWallet(wtx, iwallet.NewAddress("tmJKrg3gS4sPS7gSJ4vT8dFeqkGtfnDW4gu", iwallet.CtZCash), iwallet.FlNormal)
	if err != nil {
		t.Fatal(err)
	}

	expected := "8b6f930a2a21fa9c991989b192bc383a482a5897d899b680f0451db8be503e0a"
	if txid.String() != expected {
		t.Errorf("Expected txid %s, got %s", expected, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtZCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestZCashWallet_EstimateEscrowFee(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	tests := []struct {
		threshold int
		level     iwallet.FeeLevel
		expected  iwallet.Amount
	}{
		{
			threshold: 1,
			level:     iwallet.FlEconomic,
			expected:  iwallet.NewAmount(990),
		},
		{
			threshold: 1,
			level:     iwallet.FlNormal,
			expected:  iwallet.NewAmount(1980),
		},
		{
			threshold: 1,
			level:     iwallet.FlPriority,
			expected:  iwallet.NewAmount(3960),
		},
		{
			threshold: 2,
			level:     iwallet.FlEconomic,
			expected:  iwallet.NewAmount(1660),
		},
		{
			threshold: 2,
			level:     iwallet.FlNormal,
			expected:  iwallet.NewAmount(3320),
		},
		{
			threshold: 2,
			level:     iwallet.FlPriority,
			expected:  iwallet.NewAmount(6640),
		},
	}

	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}
	for i, test := range tests {
		fee, err := w.EstimateEscrowFee(test.threshold, test.level)
		if err != nil {
			t.Errorf("Test %d: error %s", i, err)
		}
		if fee.Cmp(test.expected) != 0 {
			t.Errorf("Test %d: expected %s, got %s", i, test.expected, fee)
		}
	}
}

func TestZCashWallet_Multisig1of2(t *testing.T) {
	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	key1Bytes, err := hex.DecodeString("84c8a01a81bf562aafafd4a9fccda533b33d6382b984c081a8cb7817bf909c18")
	if err != nil {
		t.Fatal(err)
	}

	key2Bytes, err := hex.DecodeString("c68ab7796c52952a062b4c875c758ae3831448240fb58c152cc58a224d6ad3b8")
	if err != nil {
		t.Fatal(err)
	}

	key1, _ := btcec.PrivKeyFromBytes(btcec.S256(), key1Bytes)
	key2, _ := btcec.PrivKeyFromBytes(btcec.S256(), key2Bytes)

	address, redeemScript, err := w.CreateMultisigAddress([]btcec.PublicKey{*key1.PubKey(), *key2.PubKey()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	expectedAddr := "t2VjrjNPjoPXDdgYM3PW3hTsh572EghfUQw"
	if address.String() != expectedAddr {
		t.Errorf("Expected address %s, got %s", expectedAddr, address)
	}
	expectedRedeemScript := "5121031f0ab385f3493b1e750f03ba590df5c7895415446d1c8aa60a7effc658ae183b2103c46f902f37e852dc7e8958bb440af7795fb323be6aaa3e99423076dc076315d052ae"
	if hex.EncodeToString(redeemScript) != expectedRedeemScript {
		t.Errorf("Expected redeem script %s, got %s", expectedRedeemScript, hex.EncodeToString(redeemScript))
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	tx := iwallet.Transaction{
		From: []iwallet.SpendInfo{
			{
				ID:     serializeOutpoint(op),
				Amount: iwallet.NewAmount(1000000),
			},
		},
		To: []iwallet.SpendInfo{
			{
				Amount:  iwallet.NewAmount(900000),
				Address: iwallet.NewAddress("tmJKrg3gS4sPS7gSJ4vT8dFeqkGtfnDW4gu", iwallet.CtZCash),
			},
		},
	}

	sig, err := w.SignMultisigTransaction(tx, *key1, redeemScript)
	if err != nil {
		t.Fatal(err)
	}

	wtx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w.BuildAndSend(wtx, tx, [][]iwallet.EscrowSignature{sig}, redeemScript)
	if err != nil {
		t.Fatal(err)
	}
	expectedTxid := "c9a46c5cf69295a89fec617493c6b8313b1758de59815297a844c75549aeb0fd"
	if txid.String() != expectedTxid {
		t.Errorf("Expected txid %s, got %s", expectedTxid, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtZCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestZCashWallet_Multisig2of3(t *testing.T) {
	w1, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	key1Bytes, err := hex.DecodeString("84c8a01a81bf562aafafd4a9fccda533b33d6382b984c081a8cb7817bf909c18")
	if err != nil {
		t.Fatal(err)
	}

	key2Bytes, err := hex.DecodeString("c68ab7796c52952a062b4c875c758ae3831448240fb58c152cc58a224d6ad3b8")
	if err != nil {
		t.Fatal(err)
	}

	key3Bytes, err := hex.DecodeString("0404e6967fc6c638564d4c381e299636fd01fdbcaaaa28e540647c928b44d39b")
	if err != nil {
		t.Fatal(err)
	}

	key1, _ := btcec.PrivKeyFromBytes(btcec.S256(), key1Bytes)
	key2, _ := btcec.PrivKeyFromBytes(btcec.S256(), key2Bytes)
	key3, _ := btcec.PrivKeyFromBytes(btcec.S256(), key3Bytes)

	address, redeemScript, err := w1.CreateMultisigAddress([]btcec.PublicKey{*key1.PubKey(), *key2.PubKey(), *key3.PubKey()}, 2)
	if err != nil {
		t.Fatal(err)
	}
	expectedAddr := "t2LrMZoDJmjB4gafSnPabnwmXZ6BmKSBspv"
	if address.String() != expectedAddr {
		t.Errorf("Expected address %s, got %s", expectedAddr, address)
	}
	expectedRedeemScript := "5221031f0ab385f3493b1e750f03ba590df5c7895415446d1c8aa60a7effc658ae183b2103c46f902f37e852dc7e8958bb440af7795fb323be6aaa3e99423076dc076315d02102567a15f95333dbed4ff2913e58f554d784cf7787650e44d6b7faf30c79e5b67953ae"
	if hex.EncodeToString(redeemScript) != expectedRedeemScript {
		t.Errorf("Expected redeem script %s, got %s", expectedRedeemScript, hex.EncodeToString(redeemScript))
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	tx := iwallet.Transaction{
		From: []iwallet.SpendInfo{
			{
				ID:     serializeOutpoint(op),
				Amount: iwallet.NewAmount(1000000),
			},
		},
		To: []iwallet.SpendInfo{
			{
				Amount:  iwallet.NewAmount(900000),
				Address: iwallet.NewAddress("tmJKrg3gS4sPS7gSJ4vT8dFeqkGtfnDW4gu", iwallet.CtZCash),
			},
		},
	}

	sig1, err := w1.SignMultisigTransaction(tx, *key1, redeemScript)
	if err != nil {
		t.Fatal(err)
	}

	sig2, err := w2.SignMultisigTransaction(tx, *key2, redeemScript)
	if err != nil {
		t.Fatal(err)
	}

	wtx, err := w1.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w1.BuildAndSend(wtx, tx, [][]iwallet.EscrowSignature{sig1, sig2}, redeemScript)
	if err != nil {
		t.Fatal(err)
	}
	expectedTxid := "4917289d31292eb8ef8d0ebddde57002d7dad71dea25ae5ecb9bd783dc5a3ed6"
	if txid.String() != expectedTxid {
		t.Errorf("Expected txid %s, got %s", expectedTxid, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	err = w1.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtZCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestZCashWallet_buildTx(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	b := make([]byte, 32)
	rand.Read(b)

	h, err := chainhash.NewHash(b)
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtZCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(serializeOutpoint(op)),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	b = make([]byte, 20)
	rand.Read(b)

	payTo, err := btcutil.NewAddressPubKeyHash(b, w.params())
	if err != nil {
		t.Fatal(err)
	}

	var (
		tx     *wire.MsgTx
		outVal = int64(500000)
	)
	err = w.DB.View(func(dbtx database.Tx) error {
		tx, err = w.buildTx(dbtx, outVal, iwallet.NewAddress(payTo.String(), iwallet.CtZCash), iwallet.FlNormal)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.TxIn) != 1 {
		t.Errorf("Expected 1 input got %d", len(tx.TxIn))
	}
	if tx.TxIn[0].PreviousOutPoint.String() != op.String() {
		t.Errorf("Incorrect input. Expected %s, got %s", op, tx.TxIn[0].PreviousOutPoint.String())
	}
	if len(tx.TxOut) != 2 {
		t.Errorf("Expected 2 outputs got %d", len(tx.TxOut))
	}
	paysTo := false
	script, err := txscript.PayToAddrScript(payTo)
	if err != nil {
		t.Fatal(err)
	}
	var totalOut int64
	for _, out := range tx.TxOut {
		totalOut += out.Value
		if bytes.Equal(script, out.PkScript) {
			if out.Value != outVal {
				t.Errorf("Expected out value %d got %d", outVal, out.Value)
			}
			paysTo = true
		}
	}
	if !paysTo {
		t.Error("Pay to address not found in transaction")
	}
	if totalOut != 999250 {
		t.Errorf("Expected totalOut of %d, got %d", 999250, totalOut)
	}
}

func buildTestTx() (*wire.MsgTx, []byte, error) {
	expected, err := hex.DecodeString(`0400008085202f8901a8c685478265f4c14dada651969c45a65e1aeb8cd6791f2f5bb6a1d9952104d9010000006b483045022100a61e5d557568c2ddc1d9b03a7173c6ce7c996c4daecab007ac8f34bee01e6b9702204d38fdc0bcf2728a69fde78462a10fb45a9baa27873e6a5fc45fb5c76764202a01210365ffea3efa3908918a8b8627724af852fc9b86d7375b103ab0543cf418bcaa7ffeffffff02005a6202000000001976a9148132712c3ff19f3a151234616777420a6d7ef22688ac8b959800000000001976a9145453e4698f02a38abdaa521cd1ff2dee6fac187188ac29b0040048b004000000000000000000000000`)
	if err != nil {
		return nil, nil, err
	}

	tx := wire.NewMsgTx(1)

	inHash, err := hex.DecodeString("a8c685478265f4c14dada651969c45a65e1aeb8cd6791f2f5bb6a1d9952104d9")
	if err != nil {
		return nil, nil, err
	}
	prevHash, err := chainhash.NewHash(inHash)
	if err != nil {
		return nil, nil, err
	}
	op := wire.NewOutPoint(prevHash, 1)

	scriptSig, err := hex.DecodeString("483045022100a61e5d557568c2ddc1d9b03a7173c6ce7c996c4daecab007ac8f34bee01e6b9702204d38fdc0bcf2728a69fde78462a10fb45a9baa27873e6a5fc45fb5c76764202a01210365ffea3efa3908918a8b8627724af852fc9b86d7375b103ab0543cf418bcaa7f")
	if err != nil {
		return nil, nil, err
	}
	txIn := wire.NewTxIn(op, scriptSig, nil)
	txIn.Sequence = 4294967294

	tx.TxIn = []*wire.TxIn{txIn}

	pkScirpt0, err := hex.DecodeString("76a9148132712c3ff19f3a151234616777420a6d7ef22688ac")
	if err != nil {
		return nil, nil, err
	}
	out0 := wire.NewTxOut(40000000, pkScirpt0)

	pkScirpt1, err := hex.DecodeString("76a9145453e4698f02a38abdaa521cd1ff2dee6fac187188ac")
	if err != nil {
		return nil, nil, err
	}
	out1 := wire.NewTxOut(9999755, pkScirpt1)
	tx.TxOut = []*wire.TxOut{out0, out1}

	tx.LockTime = 307241
	return tx, expected, nil
}

func TestSerializeVersion4Transaction(t *testing.T) {
	tx, expected, err := buildTestTx()
	if err != nil {
		t.Fatal(err)
	}

	serialized, err := serializeVersion4Transaction(tx, 307272)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(serialized, expected) {
		t.Fatal("Failed to serialize transaction correctly")
	}
}

func TestCalcSignatureHash(t *testing.T) {
	tx, _, err := buildTestTx()
	if err != nil {
		t.Fatal(err)
	}

	prevScript, err := hex.DecodeString("76a914507173527b4c3318a2aecd793bf1cfed705950cf88ac")
	if err != nil {
		t.Fatal(err)
	}
	sigHash, err := calcSignatureHash(prevScript, txscript.SigHashAll, tx, 0, 50000000, 307272)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := hex.DecodeString("f3148f80dfab5e573d5edfe7a850f5fd39234f80b5429d3a57edcc11e34c585b")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sigHash, expected) {
		t.Fatal("Failed to calculate correct sig hash")
	}
}
