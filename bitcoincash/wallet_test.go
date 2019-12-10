package bitcoincash

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/cpacia/multiwallet/base"
	"github.com/cpacia/multiwallet/database"
	"github.com/cpacia/multiwallet/database/sqlitedb"
	iwallet "github.com/cpacia/wallet-interface"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/op/go-logging"
	"testing"
	"time"
)

func newTestWallet() (*BitcoinCashWallet, error) {
	w := &BitcoinCashWallet{
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
	w.CoinType = iwallet.CtBitcoinCash
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

func TestBitcoinCashWallet_ValidateAddress(t *testing.T) {
	tests := []struct {
		address iwallet.Address
		valid   bool
	}{
		{
			address: iwallet.NewAddress("abc", iwallet.CtBitcoinCash),
			valid:   false,
		},
		{
			address: iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash),
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
			t.Errorf("Test %d expected valid address got invalid", i)
		}
	}
}

func TestBitcoinCashWallet_IsDust(t *testing.T) {
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

func TestBitcoinCashWallet_EstimateSpendFee(t *testing.T) {
	tests := []struct {
		feeLevel      iwallet.FeeLevel
		amount        iwallet.Amount
		expected      iwallet.Amount
		expectedError error
	}{
		{
			amount:   iwallet.NewAmount(500000),
			feeLevel: iwallet.FlEconomic,
			expected: iwallet.NewAmount(1135),
		},
		{
			amount:   iwallet.NewAmount(500000),
			feeLevel: iwallet.FlNormal,
			expected: iwallet.NewAmount(3405),
		},
		{
			amount:   iwallet.NewAmount(500000),
			feeLevel: iwallet.FlPriority,
			expected: iwallet.NewAmount(5675),
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

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtBitcoinCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(buf.Bytes()),
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

func TestBitcoinCashWallet_BuildAndSend(t *testing.T) {
	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	fromAddr, err := bchutil.DecodeAddress(addr.String(), &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(fromAddr)
	if err != nil {
		t.Fatal(err)
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtBitcoinCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(buf.Bytes()),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	wtx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w.Spend(wtx, iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash), iwallet.NewAmount(500000), iwallet.FlNormal)
	if err != nil {
		t.Fatal(err)
	}

	expected := "4922d84a573b1fadbe2cbdaf907aab99f0d89c67e106fdd86277c5e4224925fc"
	if txid.String() != expected {
		t.Errorf("Expected txid %s, got %s", txid, expected)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	var txBytes []byte
	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtBitcoinCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		txBytes = txs[0].TxBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var tx wire.MsgTx
	if err := tx.BchDecode(bytes.NewReader(txBytes), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	vm, err := txscript.NewEngine(fromScript, &tx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}

func TestBitcoinCashWallet_SweepWallet(t *testing.T) {
	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	fromAddr, err := bchutil.DecodeAddress(addr.String(), &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(fromAddr)
	if err != nil {
		t.Fatal(err)
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtBitcoinCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(buf.Bytes()),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	wtx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w.SweepWallet(wtx, iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash), iwallet.FlNormal)
	if err != nil {
		t.Fatal(err)
	}

	expected := "dfb615c1514d41198920a669616b0ff3ff3c7f3067d44f9209280f3523d28558"
	if txid.String() != expected {
		t.Errorf("Expected txid %s, got %s", txid, expected)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	var txBytes []byte
	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtBitcoinCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		txBytes = txs[0].TxBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var tx wire.MsgTx
	if err := tx.BchDecode(bytes.NewReader(txBytes), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	vm, err := txscript.NewEngine(fromScript, &tx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}

func TestBitcoinCashWallet_EstimateEscrowFee(t *testing.T) {
	tests := []struct {
		threshold int
		level     iwallet.FeeLevel
		expected  iwallet.Amount
	}{
		{
			threshold: 1,
			level:     iwallet.FlEconomic,
			expected:  iwallet.NewAmount(915),
		},
		{
			threshold: 1,
			level:     iwallet.FlNormal,
			expected:  iwallet.NewAmount(2745),
		},
		{
			threshold: 1,
			level:     iwallet.FlPriority,
			expected:  iwallet.NewAmount(4575),
		},
		{
			threshold: 2,
			level:     iwallet.FlEconomic,
			expected:  iwallet.NewAmount(1585),
		},
		{
			threshold: 2,
			level:     iwallet.FlNormal,
			expected:  iwallet.NewAmount(4755),
		},
		{
			threshold: 2,
			level:     iwallet.FlPriority,
			expected:  iwallet.NewAmount(7925),
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

func TestBitcoinCashWallet_Multisig1of2(t *testing.T) {
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
	expectedAddr := "prlxr3xvattzez7y79k5yv4gtgrqlxthyc9dnv8mm4"
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

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	tx := iwallet.Transaction{
		From: []iwallet.SpendInfo{
			{
				ID:     buf.Bytes(),
				Amount: iwallet.NewAmount(1000000),
			},
		},
		To: []iwallet.SpendInfo{
			{
				Amount:  iwallet.NewAmount(900000),
				Address: iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash),
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
	expectedTxid := "0f103a079ca2b0252e47a557d4c2aeb908d5570ba0907ef52512ce4740c49bac"
	if txid.String() != expectedTxid {
		t.Errorf("Expected txid %s, got %s", expectedTxid, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	var txBytes []byte
	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtBitcoinCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		txBytes = txs[0].TxBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	scriptAddr, err := bchutil.NewAddressScriptHash(redeemScript, w.params())
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(scriptAddr)
	if err != nil {
		t.Fatal(err)
	}

	var msgTx wire.MsgTx
	if err := msgTx.BchDecode(bytes.NewReader(txBytes), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	vm, err := txscript.NewEngine(fromScript, &msgTx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}

func TestBitcoinCashWallet_Multisig2of3(t *testing.T) {
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
	expectedAddr := "pzwwxvlrywdy0gkzaq6ttxkccfznxw3sqsf42jhhea"
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

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	tx := iwallet.Transaction{
		From: []iwallet.SpendInfo{
			{
				ID:     buf.Bytes(),
				Amount: iwallet.NewAmount(1000000),
			},
		},
		To: []iwallet.SpendInfo{
			{
				Amount:  iwallet.NewAmount(900000),
				Address: iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash),
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
	expectedTxid := "a0a6487eaa732903b5344fc864dd6c33a00b7df3ec87dc2c0e341151495c325a"
	if txid.String() != expectedTxid {
		t.Errorf("Expected txid %s, got %s", expectedTxid, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	var txBytes []byte
	err = w1.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtBitcoinCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		txBytes = txs[0].TxBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	scriptAddr, err := bchutil.NewAddressScriptHash(redeemScript, w1.params())
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(scriptAddr)
	if err != nil {
		t.Fatal(err)
	}

	var msgTx wire.MsgTx
	if err := msgTx.BchDecode(bytes.NewReader(txBytes), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	vm, err := txscript.NewEngine(fromScript, &msgTx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}

func TestBitcoinCashWallet_Multisig2of3Timlocked(t *testing.T) {
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

	address, redeemScript, err := w1.CreateMultisigWithTimeout([]btcec.PublicKey{*key1.PubKey(), *key2.PubKey(), *key3.PubKey()}, 2, time.Hour*24, *key2.PubKey())
	if err != nil {
		t.Fatal(err)
	}
	expectedAddr := "pr62804de6uwc42w0ktf64znavkfaa0eyujm08xlwx"
	if address.String() != expectedAddr {
		t.Errorf("Expected address %s, got %s", expectedAddr, address)
	}
	expectedRedeemScript := "635221031f0ab385f3493b1e750f03ba590df5c7895415446d1c8aa60a7effc658ae183b2103c46f902f37e852dc7e8958bb440af7795fb323be6aaa3e99423076dc076315d02102567a15f95333dbed4ff2913e58f554d784cf7787650e44d6b7faf30c79e5b67953ae67029000b2752103c46f902f37e852dc7e8958bb440af7795fb323be6aaa3e99423076dc076315d0ac68"
	if hex.EncodeToString(redeemScript) != expectedRedeemScript {
		t.Errorf("Expected redeem script %s, got %s", expectedRedeemScript, hex.EncodeToString(redeemScript))
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	tx := iwallet.Transaction{
		From: []iwallet.SpendInfo{
			{
				ID:     buf.Bytes(),
				Amount: iwallet.NewAmount(1000000),
			},
		},
		To: []iwallet.SpendInfo{
			{
				Amount:  iwallet.NewAmount(900000),
				Address: iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash),
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
	expectedTxid := "ea33ac8c7361268976c2e56a62136fd0dc819828de0243fe5535a2ff6e5c87e7"
	if txid.String() != expectedTxid {
		t.Errorf("Expected txid %s, got %s", expectedTxid, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	var txBytes []byte
	err = w1.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtBitcoinCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		txBytes = txs[0].TxBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	scriptAddr, err := bchutil.NewAddressScriptHash(redeemScript, w1.params())
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(scriptAddr)
	if err != nil {
		t.Fatal(err)
	}

	var msgTx wire.MsgTx
	if err := msgTx.BchDecode(bytes.NewReader(txBytes), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	vm, err := txscript.NewEngine(fromScript, &msgTx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}

func TestBitcoinCashWallet_ReleaseFundsAfterTimeout(t *testing.T) {
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

	key3Bytes, err := hex.DecodeString("0404e6967fc6c638564d4c381e299636fd01fdbcaaaa28e540647c928b44d39b")
	if err != nil {
		t.Fatal(err)
	}

	key1, _ := btcec.PrivKeyFromBytes(btcec.S256(), key1Bytes)
	key2, _ := btcec.PrivKeyFromBytes(btcec.S256(), key2Bytes)
	key3, _ := btcec.PrivKeyFromBytes(btcec.S256(), key3Bytes)

	address, redeemScript, err := w.CreateMultisigWithTimeout([]btcec.PublicKey{*key1.PubKey(), *key2.PubKey(), *key3.PubKey()}, 2, time.Hour*24, *key2.PubKey())
	if err != nil {
		t.Fatal(err)
	}
	expectedAddr := "pr62804de6uwc42w0ktf64znavkfaa0eyujm08xlwx"
	if address.String() != expectedAddr {
		t.Errorf("Expected address %s, got %s", expectedAddr, address)
	}
	expectedRedeemScript := "635221031f0ab385f3493b1e750f03ba590df5c7895415446d1c8aa60a7effc658ae183b2103c46f902f37e852dc7e8958bb440af7795fb323be6aaa3e99423076dc076315d02102567a15f95333dbed4ff2913e58f554d784cf7787650e44d6b7faf30c79e5b67953ae67029000b2752103c46f902f37e852dc7e8958bb440af7795fb323be6aaa3e99423076dc076315d0ac68"
	if hex.EncodeToString(redeemScript) != expectedRedeemScript {
		t.Errorf("Expected redeem script %s, got %s", expectedRedeemScript, hex.EncodeToString(redeemScript))
	}

	h, err := chainhash.NewHashFromStr("bdb237bf8c5de6b60ba1e2dcfe364fc24f583e568d1682f851a9d0f11a45c78d")
	if err != nil {
		t.Fatal(err)
	}

	op := wire.NewOutPoint(h, 0)

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	tx := iwallet.Transaction{
		From: []iwallet.SpendInfo{
			{
				ID:     buf.Bytes(),
				Amount: iwallet.NewAmount(1000000),
			},
		},
		To: []iwallet.SpendInfo{
			{
				Amount:  iwallet.NewAmount(900000),
				Address: iwallet.NewAddress("qrk0e04s67l9mf20jvae6fznht04rej57sf8jz2nua", iwallet.CtBitcoinCash),
			},
		},
	}

	wtx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txid, err := w.ReleaseFundsAfterTimeout(wtx, tx, *key2, redeemScript)
	if err != nil {
		t.Fatal(err)
	}
	expectedTxid := "81b911ff25c1b3acb68d5754d59607510fe11693e4957427504f559233fd7c2b"
	if txid.String() != expectedTxid {
		t.Errorf("Expected txid %s, got %s", expectedTxid, txid)
	}

	if err := wtx.Commit(); err != nil {
		t.Fatal(err)
	}

	var txBytes []byte
	err = w.DB.View(func(tx database.Tx) error {
		var txs []database.UnconfirmedTransaction
		if err := tx.Read().Where("coin=?", iwallet.CtBitcoinCash).Find(&txs).Error; err != nil {
			return err
		}
		if len(txs) != 1 {
			t.Errorf("Expected 1 tx found %d", len(txs))
		}
		if txs[0].Txid != txid.String() {
			t.Errorf("Expected txid %s, got %s", txid, txs[0].Txid)
		}
		txBytes = txs[0].TxBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	scriptAddr, err := bchutil.NewAddressScriptHash(redeemScript, w.params())
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(scriptAddr)
	if err != nil {
		t.Fatal(err)
	}

	var msgTx wire.MsgTx
	if err := msgTx.BchDecode(bytes.NewReader(txBytes), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	vm, err := txscript.NewEngine(fromScript, &msgTx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}

func TestBitcoinCashWallet_buildTx(t *testing.T) {
	w, err := newTestWallet()
	if err != nil {
		t.Fatal(err)
	}

	addr, err := w.Keychain.CurrentAddress(false)
	if err != nil {
		t.Fatal(err)
	}

	fromAddr, err := bchutil.DecodeAddress(addr.String(), &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}

	fromScript, err := txscript.PayToAddrScript(fromAddr)
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

	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	err = w.DB.Update(func(tx database.Tx) error {
		return tx.Save(&database.UtxoRecord{
			Timestamp: time.Now(),
			Amount:    "1000000",
			Height:    600000,
			Coin:      iwallet.CtBitcoinCash,
			Address:   addr.String(),
			Outpoint:  hex.EncodeToString(buf.Bytes()),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	b = make([]byte, 20)
	rand.Read(b)

	payTo, err := bchutil.NewAddressPubKeyHash(b, &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}

	var (
		tx     *wire.MsgTx
		outVal = int64(500000)
	)
	err = w.DB.View(func(dbtx database.Tx) error {
		tx, err = w.buildTx(dbtx, outVal, iwallet.NewAddress(payTo.String(), iwallet.CtBitcoinCash), iwallet.FlNormal)
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
	if totalOut != 996595 {
		t.Errorf("Expected totalOut of %d, got %d", 996595, totalOut)
	}

	vm, err := txscript.NewEngine(fromScript, tx, 0, txscript.StandardVerifyFlags, nil, nil, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Errorf("Script verificationf failed: %s", err)
	}
}
