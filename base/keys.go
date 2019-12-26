package base

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/cpacia/multiwallet/database"
	iwallet "github.com/cpacia/wallet-interface"
	"github.com/jinzhu/gorm"
	"golang.org/x/crypto/pbkdf2"
	"io"
	"sync"
	"time"
)

const (
	// defaultLookaheadWindow is the number of keys to generate after the last
	// unused key in the wallet. The key manager strives to maintain
	// this buffer.
	defaultLookaheadWindow = 10

	// defaultKdfRounds is the number of rounds to use when generating the
	// encryption key. The greater this number is, the harder it is to
	// brute force the encryption key.
	defaultKdfRounds = 8192

	// defaultKeyLength is the encryption key length generated by pbkdf2.
	defaultKeyLength = 32
)

// ErrEncryptedKeychain means the keychain is encrypted.
var ErrEncryptedKeychain = errors.New("keychain is encrypted")

// KeychainConfig holds some optional configuration options for
// the keychain.
type KeychainConfig struct {
	LookaheadWindowSize int
	ExternalOnly        bool
	DisableMarkAsUsed   bool
}

// Apply applies the given options to this Option
func (cfg *KeychainConfig) Apply(opts ...KeychainOption) error {
	for i, opt := range opts {
		if err := opt(cfg); err != nil {
			return fmt.Errorf("keychain option %d failed: %s", i, err)
		}
	}
	return nil
}

// KeychainOption is a keychain option type.
type KeychainOption func(*KeychainConfig) error

// Keychain manages a Bip44 keychain for each coin.
type Keychain struct {
	db              database.Database
	internalPrivkey *hd.ExtendedKey
	internalPubkey  *hd.ExtendedKey

	externalPrivkey *hd.ExtendedKey
	externalPubkey  *hd.ExtendedKey

	lookaheadWindowSize int
	externalOnly        bool
	disableMarkAsUsed   bool

	coinType iwallet.CoinType

	mtx sync.RWMutex

	addrFunc func(key *hd.ExtendedKey) (iwallet.Address, error)
}

// NewKeychain instantiates a new Keychain for the given coin with the provided keys.
//
// Note the following derivation path used by the Keychain:
// Typical Bip44 derivation is:
//
// m / purpose' / coin_type' / account' / change / address_index
//
// For our purpose we only store the `account` level so as to prevent this class from
// deriving keys for other coins. Further, we only generate addresses using the master
// public key keys so we do not need the master private key to generate new addresses.
// This allows us to encrypt the master private key if the user desires.
func NewKeychain(db database.Database, coinType iwallet.CoinType, addressFunc func(key *hd.ExtendedKey) (iwallet.Address, error), opts ...KeychainOption) (*Keychain, error) {
	cfg := KeychainConfig{LookaheadWindowSize: defaultLookaheadWindow}
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}
	var (
		externalPrivkey, externalPubkey, internalPrivkey, internalPubkey *hd.ExtendedKey
		coinRecord                                                       database.CoinRecord
	)
	err := db.View(func(tx database.Tx) error {
		return tx.Read().Where("coin=?", coinType.CurrencyCode()).Find(&coinRecord).Error
	})
	if err != nil {
		return nil, err
	}
	accountPubKey, err := hd.NewKeyFromString(coinRecord.MasterPub)
	if err != nil {
		return nil, err
	}

	if !coinRecord.EncryptedMasterKey {
		accountPrivKey, err := hd.NewKeyFromString(coinRecord.MasterPriv)
		if err != nil {
			return nil, err
		}
		externalPrivkey, internalPrivkey, err = generateAccountPrivKeys(accountPrivKey)
		if err != nil {
			return nil, err
		}
		externalPubkey, internalPubkey, err = generateAccountPubKeys(accountPubKey)
		if err != nil {
			return nil, err
		}
	} else {
		externalPubkey, internalPubkey, err = generateAccountPubKeys(accountPubKey)
		if err != nil {
			return nil, err
		}
	}

	kc := &Keychain{
		db:                  db,
		internalPrivkey:     internalPrivkey,
		internalPubkey:      internalPubkey,
		externalPrivkey:     externalPrivkey,
		externalPubkey:      externalPubkey,
		lookaheadWindowSize: cfg.LookaheadWindowSize,
		externalOnly:        cfg.ExternalOnly,
		disableMarkAsUsed:   cfg.DisableMarkAsUsed,
		coinType:            coinType,
		addrFunc:            addressFunc,
		mtx:                 sync.RWMutex{},
	}
	if err := kc.ExtendKeychain(); err != nil {
		return nil, err
	}
	return kc, nil
}

// SetPassphase encrypts the master private key in the database and
// deletes the internal and external private keys from memory.
func (kc *Keychain) SetPassphase(pw []byte) error {
	kc.mtx.Lock()
	defer kc.mtx.Unlock()

	var (
		salt       = make([]byte, 32)
		rounds     = defaultKdfRounds
		keyLen     = defaultKeyLength
		coinRecord database.CoinRecord
	)

	return kc.db.Update(func(tx database.Tx) error {
		err := tx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Find(&coinRecord).Error
		if err != nil {
			return err
		}

		if coinRecord.EncryptedMasterKey {
			return errors.New("keychain already encrypted")
		}

		plaintext := []byte(coinRecord.MasterPriv)

		_, err = rand.Read(salt)
		if err != nil {
			return err
		}
		dk := pbkdf2.Key(pw, salt, rounds, keyLen, sha512.New)

		block, err := aes.NewCipher(dk)
		if err != nil {
			return err
		}

		// The IV needs to be unique, but not secure. Therefore it's common to
		// include it at the beginning of the ciphertext.
		ciphertext := make([]byte, aes.BlockSize+len(plaintext))
		iv := ciphertext[:aes.BlockSize]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return err
		}

		stream := cipher.NewCFBEncrypter(block, iv)
		stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)

		coinRecord.MasterPriv = base64.StdEncoding.EncodeToString(ciphertext)
		coinRecord.EncryptedMasterKey = true
		coinRecord.KdfRounds = rounds
		coinRecord.KdfKeyLen = keyLen
		coinRecord.Salt = salt

		kc.externalPrivkey = nil
		kc.internalPrivkey = nil

		return tx.Save(&coinRecord)
	})
}

// ChangePassphrase will change the passphrase used to encrypt the
// master private key.
func (kc *Keychain) ChangePassphrase(old, new []byte) error {
	kc.mtx.Lock()
	defer kc.mtx.Unlock()

	if kc.internalPrivkey != nil || kc.externalPrivkey != nil {
		return errors.New("wallet is not encrypted")
	}

	var (
		salt       = make([]byte, 32)
		rounds     = defaultKdfRounds
		keyLen     = defaultKeyLength
		coinRecord database.CoinRecord
	)

	return kc.db.Update(func(tx database.Tx) error {
		err := tx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Find(&coinRecord).Error
		if err != nil {
			return err
		}

		ciphertext, err := base64.StdEncoding.DecodeString(coinRecord.MasterPriv)
		if err != nil {
			return err
		}

		dk := pbkdf2.Key(old, coinRecord.Salt, coinRecord.KdfRounds, coinRecord.KdfKeyLen, sha512.New)

		block, err := aes.NewCipher(dk)
		if err != nil {
			return err
		}

		// The IV needs to be unique, but not secure. Therefore it's common to
		// include it at the beginning of the ciphertext.
		if len(ciphertext) < aes.BlockSize {
			return errors.New("ciphertext too short")
		}
		iv := ciphertext[:aes.BlockSize]
		ciphertext = ciphertext[aes.BlockSize:]

		stream := cipher.NewCFBDecrypter(block, iv)

		// XORKeyStream can work in-place if the two arguments are the same.
		stream.XORKeyStream(ciphertext, ciphertext)

		plaintext := ciphertext

		_, err = hd.NewKeyFromString(string(plaintext))
		if err != nil {
			return errors.New("invalid passphrase")
		}

		_, err = rand.Read(salt)
		if err != nil {
			return err
		}

		dk = pbkdf2.Key(new, salt, rounds, keyLen, sha512.New)

		block, err = aes.NewCipher(dk)
		if err != nil {
			return err
		}

		// The IV needs to be unique, but not secure. Therefore it's common to
		// include it at the beginning of the ciphertext.
		ciphertext = make([]byte, aes.BlockSize+len(plaintext))
		iv = ciphertext[:aes.BlockSize]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return err
		}

		stream = cipher.NewCFBEncrypter(block, iv)
		stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)

		coinRecord.MasterPriv = base64.StdEncoding.EncodeToString(ciphertext)
		coinRecord.EncryptedMasterKey = true
		coinRecord.KdfRounds = rounds
		coinRecord.KdfKeyLen = keyLen
		coinRecord.Salt = salt

		return tx.Save(&coinRecord)
	})
}

// RemovePassphrase removes encryption from the master key and puts the
// external and internal keys back in memory.
func (kc *Keychain) RemovePassphrase(pw []byte) error {
	kc.mtx.Lock()
	defer kc.mtx.Unlock()

	if kc.internalPrivkey != nil || kc.externalPrivkey != nil {
		return errors.New("wallet is not encrypted")
	}

	return kc.db.Update(func(tx database.Tx) error {
		var coinRecord database.CoinRecord
		err := tx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Find(&coinRecord).Error
		if err != nil {
			return err
		}

		ciphertext, err := base64.StdEncoding.DecodeString(coinRecord.MasterPriv)
		if err != nil {
			return err
		}

		dk := pbkdf2.Key(pw, coinRecord.Salt, coinRecord.KdfRounds, coinRecord.KdfKeyLen, sha512.New)

		block, err := aes.NewCipher(dk)
		if err != nil {
			return err
		}

		// The IV needs to be unique, but not secure. Therefore it's common to
		// include it at the beginning of the ciphertext.
		if len(ciphertext) < aes.BlockSize {
			return errors.New("ciphertext too short")
		}
		iv := ciphertext[:aes.BlockSize]
		ciphertext = ciphertext[aes.BlockSize:]

		stream := cipher.NewCFBDecrypter(block, iv)

		// XORKeyStream can work in-place if the two arguments are the same.
		stream.XORKeyStream(ciphertext, ciphertext)

		key, err := hd.NewKeyFromString(string(ciphertext))
		if err != nil {
			return errors.New("invalid passphrase")
		}

		kc.externalPrivkey, kc.internalPrivkey, err = generateAccountPrivKeys(key)
		if err != nil {
			return err
		}

		coinRecord.MasterPriv = string(ciphertext)
		coinRecord.EncryptedMasterKey = false

		return tx.Save(&coinRecord)
	})
}

// Unlock will dcrypt the master key and store the external and internal
// private keys in memory for howLong.
func (kc *Keychain) Unlock(pw []byte, howLong time.Duration) error {
	kc.mtx.Lock()
	defer kc.mtx.Unlock()

	if kc.internalPrivkey != nil || kc.externalPrivkey != nil {
		return errors.New("wallet is not encrypted")
	}

	var coinRecord database.CoinRecord
	err := kc.db.View(func(tx database.Tx) error {
		return tx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Find(&coinRecord).Error
	})
	if err != nil {
		return err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(coinRecord.MasterPriv)
	if err != nil {
		return err
	}

	dk := pbkdf2.Key(pw, coinRecord.Salt, coinRecord.KdfRounds, coinRecord.KdfKeyLen, sha512.New)

	block, err := aes.NewCipher(dk)
	if err != nil {
		return err
	}

	// The IV needs to be unique, but not secure. Therefore it's common to
	// include it at the beginning of the ciphertext.
	if len(ciphertext) < aes.BlockSize {
		return errors.New("ciphertext too short")
	}
	iv := ciphertext[:aes.BlockSize]
	ciphertext = ciphertext[aes.BlockSize:]

	stream := cipher.NewCFBDecrypter(block, iv)

	// XORKeyStream can work in-place if the two arguments are the same.
	stream.XORKeyStream(ciphertext, ciphertext)

	key, err := hd.NewKeyFromString(string(ciphertext))
	if err != nil {
		return err
	}

	kc.externalPrivkey, kc.internalPrivkey, err = generateAccountPrivKeys(key)
	if err != nil {
		return err
	}

	time.AfterFunc(howLong, func() {
		kc.mtx.Lock()
		defer kc.mtx.Unlock()

		kc.externalPrivkey = nil
		kc.internalPrivkey = nil
	})
	return nil
}

// IsEncrypted returns whether or not this keychain is encrypted.
func (kc *Keychain) IsEncrypted() bool {
	kc.mtx.RLock()
	defer kc.mtx.RUnlock()

	return kc.internalPrivkey == nil || kc.externalPrivkey == nil
}

// GetAddresses returns all addresses in the wallet.
func (kc *Keychain) GetAddresses() ([]iwallet.Address, error) {
	var records []database.AddressRecord
	err := kc.db.Update(func(tx database.Tx) error {
		return tx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Find(&records).Error
	})
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		return nil, err
	}
	var addrs []iwallet.Address
	for _, rec := range records {
		addrs = append(addrs, rec.Address())
	}
	return addrs, nil
}

// CurrentAddress returns the first unused address.
func (kc *Keychain) CurrentAddress(change bool) (iwallet.Address, error) {
	if change && kc.externalOnly {
		return iwallet.Address{}, errors.New("keychain is configured for external addresses only")
	}
	var record database.AddressRecord
	err := kc.db.View(func(tx database.Tx) error {
		return tx.Read().Order("key_index asc").Where("coin=?", kc.coinType.CurrencyCode()).Where("used=?", false).Where("change=?", change).First(&record).Error
	})
	if err != nil {
		return iwallet.Address{}, err
	}
	return record.Address(), nil
}

// CurrentAddressWithTx returns the first unused address using an open database transasction.
func (kc *Keychain) CurrentAddressWithTx(dbtx database.Tx, change bool) (iwallet.Address, error) {
	var record database.AddressRecord
	err := dbtx.Read().Order("key_index asc").Where("coin=?", kc.coinType.CurrencyCode()).Where("used=?", false).Where("change=?", change).First(&record).Error
	if err != nil {
		return iwallet.Address{}, err
	}
	return record.Address(), nil
}

// NewAddress returns a new, never before used address.
func (kc *Keychain) NewAddress(change bool) (iwallet.Address, error) {
	var address iwallet.Address
	err := kc.db.Update(func(tx database.Tx) error {
		var record database.AddressRecord
		err := tx.Read().Order("key_index desc").Where("coin=?", kc.coinType.CurrencyCode()).Where("change=?", change).First(&record).Error
		if err != nil {
			return err
		}
		var (
			index  = record.KeyIndex + 1
			newKey *hd.ExtendedKey
		)

		for {
			newKey, err = kc.externalPubkey.Child(uint32(index))
			if err == nil {
				break
			}
			index++
		}

		address, err = kc.addrFunc(newKey)
		if err != nil {
			return err
		}

		newRecord := &database.AddressRecord{
			Addr:     address.String(),
			KeyIndex: index,
			Change:   false,
			Used:     false,
			Coin:     kc.coinType.CurrencyCode(),
		}
		if err := kc.extendKeychain(tx); err != nil {
			return err
		}
		return tx.Save(&newRecord)
	})
	return address, err
}

// HasKey returns whether or not this wallet can derive the key for
// this address.
func (kc *Keychain) HasKey(addr iwallet.Address) (bool, error) {
	has := false
	err := kc.db.View(func(tx database.Tx) error {
		var record database.AddressRecord
		err := tx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Where("addr=?", addr.String()).First(&record).Error
		if err != nil && !gorm.IsRecordNotFoundError(err) {
			return err
		} else if err == nil {
			has = true
		}
		return nil
	})
	return has, err
}

// KeyForAddress returns the private key for the given address. If this wallet is not
// encrypted then accountPrivKey may be nil and it will generate and return the key.
// However, if the wallet is encrypted a unencrypted accountPrivKey must be passed in
// so we can derive the correct child key.
func (kc *Keychain) KeyForAddress(dbtx database.Tx, addr iwallet.Address, accountPrivKey *hd.ExtendedKey) (*hd.ExtendedKey, error) {
	kc.mtx.Lock()
	defer kc.mtx.Unlock()

	var record database.AddressRecord
	err := dbtx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Where("addr=?", addr.String()).First(&record).Error
	if err != nil {
		return nil, err
	}
	var (
		key             *hd.ExtendedKey
		externalPrivkey = kc.externalPrivkey
		internalPrivkey = kc.internalPrivkey
	)

	if (externalPrivkey == nil || internalPrivkey == nil) && accountPrivKey != nil {
		externalPrivkey, internalPrivkey, err = generateAccountPrivKeys(accountPrivKey)
		if err != nil {
			return nil, err
		}
	}

	if record.Change {
		if internalPrivkey == nil {
			return nil, ErrEncryptedKeychain
		}
		key, err = internalPrivkey.Child(uint32(record.KeyIndex))
	} else {
		if externalPrivkey == nil {
			return nil, ErrEncryptedKeychain
		}
		key, err = externalPrivkey.Child(uint32(record.KeyIndex))
	}
	return key, err
}

// MarkAddressAsUsed marks the given address as used and extends the keychain.
func (kc *Keychain) MarkAddressAsUsed(dbtx database.Tx, addr iwallet.Address) error {
	if kc.disableMarkAsUsed {
		return nil
	}
	var record database.AddressRecord
	err := dbtx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Where("addr=?", addr.String()).First(&record).Error
	if err != nil {
		return err
	}
	record.Used = true

	if err := dbtx.Save(&record); err != nil {
		return err
	}

	return kc.extendKeychain(dbtx)
}

// ExtendKeychain generates a buffer of 20 unused keys after the last used
// key in both the internal and external keychains. The reason we do this
// is to increase the likelihood that we will detect all our transactions
// when restoring from seed.
//
// The typical rescan workflow is:
// 1. Extend keychain
// 2. Query for transactions
// 3. If there are any transactions returned repeat steps 1 - 3 until
// there are no more transactions returned.
func (kc *Keychain) ExtendKeychain() error {
	return kc.db.Update(func(tx database.Tx) error {
		return kc.extendKeychain(tx)
	})
}

func (kc *Keychain) extendKeychain(tx database.Tx) error {
	internalUnused, externalUnused, err := kc.getLookaheadWindows(tx)
	if err != nil {
		return err
	}
	if !kc.externalOnly {
		if internalUnused < kc.lookaheadWindowSize {
			if err := kc.createNewKeys(tx, true, kc.lookaheadWindowSize-internalUnused); err != nil {
				return err
			}
		}
	}
	if externalUnused < kc.lookaheadWindowSize {
		if err := kc.createNewKeys(tx, false, kc.lookaheadWindowSize-externalUnused); err != nil {
			return err
		}
	}
	return nil
}

func (kc *Keychain) createNewKeys(dbtx database.Tx, change bool, numKeys int) error {
	var (
		record        database.AddressRecord
		generatedKeys = 0
	)
	err := dbtx.Read().Order("key_index desc").Where("coin=?", kc.coinType.CurrencyCode()).Where("change=?", change).First(&record).Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		return err
	}
	nextIndex := record.KeyIndex + 1
	if gorm.IsRecordNotFoundError(err) {
		nextIndex = 0
	}
	for generatedKeys < numKeys {
		// There is a small possibility bip32 keys can be invalid. The procedure in such cases
		// is to discard the key and derive the next one. This loop will continue until a valid key
		// is derived.
		var newKey *hd.ExtendedKey
		if change {
			newKey, err = kc.internalPubkey.Child(uint32(nextIndex))
		} else {
			newKey, err = kc.externalPubkey.Child(uint32(nextIndex))
		}
		if err != nil {
			nextIndex++
			continue
		}

		addr, err := kc.addrFunc(newKey)
		if err != nil {
			return err
		}

		newRecord := &database.AddressRecord{
			Addr:     addr.String(),
			KeyIndex: nextIndex,
			Change:   change,
			Used:     false,
			Coin:     kc.coinType.CurrencyCode(),
		}

		if err := dbtx.Save(&newRecord); err != nil {
			return err
		}
		generatedKeys++
		nextIndex++
	}
	return nil
}

func (kc *Keychain) getLookaheadWindows(dbtx database.Tx) (internalUnused, externalUnused int, err error) {
	var addressRecords []database.AddressRecord
	rerr := dbtx.Read().Where("coin=?", kc.coinType.CurrencyCode()).Find(&addressRecords).Error
	if rerr != nil && !gorm.IsRecordNotFoundError(rerr) {
		err = rerr
		return
	}
	internalLastUsed := -1
	externalLastUsed := -1
	for _, rec := range addressRecords {
		if rec.Change && rec.Used && rec.KeyIndex > internalLastUsed {
			internalLastUsed = rec.KeyIndex
		}
		if !rec.Change && rec.Used && rec.KeyIndex > externalLastUsed {
			externalLastUsed = rec.KeyIndex
		}
	}
	for _, rec := range addressRecords {
		if rec.Change && !rec.Used && rec.KeyIndex > internalLastUsed {
			internalUnused++
		}
		if !rec.Change && !rec.Used && rec.KeyIndex > externalLastUsed {
			externalUnused++
		}
	}
	return
}

func generateAccountPrivKeys(accountPrivKey *hd.ExtendedKey) (external, internal *hd.ExtendedKey, err error) {
	// Change(0) = external
	external, err = accountPrivKey.Child(0)
	if err != nil {
		return nil, nil, err
	}
	// Change(1) = internal
	internal, err = accountPrivKey.Child(1)
	if err != nil {
		return nil, nil, err
	}
	return
}

func generateAccountPubKeys(accountPubKey *hd.ExtendedKey) (external, internal *hd.ExtendedKey, err error) {
	// Change(0) = external
	external, err = accountPubKey.Child(0)
	if err != nil {
		return nil, nil, err
	}
	// Change(1) = internal
	internal, err = accountPubKey.Child(1)
	if err != nil {
		return nil, nil, err
	}
	return
}
