package vault

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	. "github.com/nknorg/nkn/common"
	"github.com/nknorg/nkn/crypto"
	"github.com/nknorg/nkn/util/log"
	"github.com/nknorg/nkn/util/password"
	"github.com/nknorg/nkn/vm/contract"
	"github.com/nknorg/nkn/vm/signature"
)

const (
	WalletIVLength        = 16
	WalletMasterKeyLength = 32
	WalletFileName        = "wallet.dat"
)

type Wallet interface {
	Sign(context *contract.ContractContext) error
	GetAccount(pubKey *crypto.PubKey) (*Account, error)
	GetDefaultAccount() (*Account, error)
	//GetUnspent() (map[Uint256][]*transaction.UTXOUnspent, error)
}

type WalletImpl struct {
	path      string
	iv        []byte
	masterKey []byte
	account   *Account
	contract  *contract.Contract
	*WalletStore
}

func NewWallet(path string, password []byte, needAccount bool) (*WalletImpl, error) {
	var err error
	// store init
	store, err := NewStore(path)
	if err != nil {
		return nil, err
	}
	// generate password hash
	passwordKey := crypto.ToAesKey(password)
	pwdhash := sha256.Sum256(passwordKey)
	// generate IV
	iv := make([]byte, WalletIVLength)
	_, err = rand.Read(iv)
	if err != nil {
		return nil, err
	}
	// generate master key
	masterKey := make([]byte, WalletMasterKeyLength)
	_, err = rand.Read(masterKey)
	if err != nil {
		return nil, err
	}
	encryptedMasterKey, err := crypto.AesEncrypt(masterKey[:], passwordKey, iv)
	if err != nil {
		return nil, err
	}
	// persist to store
	err = store.SaveBasicData([]byte(WalletStoreVersion), iv, encryptedMasterKey, pwdhash[:])
	if err != nil {
		return nil, err
	}

	w := &WalletImpl{
		path:        path,
		iv:          iv,
		masterKey:   masterKey,
		WalletStore: store,
	}
	// generate default account
	if needAccount {
		err = w.CreateAccount(nil)
		if err != nil {
			return nil, err
		}
	}

	return w, nil
}

func OpenWallet(path string, password []byte) (*WalletImpl, error) {
	var err error
	store, err := LoadStore(path)
	if err != nil {
		return nil, err
	}

	passwordKey := crypto.ToAesKey(password)
	passwordKeyHash, err := HexStringToBytes(store.Data.PasswordHash)
	if err != nil {
		return nil, err
	}
	if ok := verifyPasswordKey(passwordKey, passwordKeyHash); !ok {
		return nil, errors.New("password wrong")
	}
	iv, err := HexStringToBytes(store.Data.IV)
	if err != nil {
		return nil, err
	}
	encryptedMasterKey, err := HexStringToBytes(store.Data.MasterKey)
	if err != nil {
		return nil, err
	}
	masterKey, err := crypto.AesDecrypt(encryptedMasterKey, passwordKey, iv)
	if err != nil {
		return nil, err
	}

	encryptedPrivateKey, err := HexStringToBytes(store.Data.AccountData.PrivateKeyEncrypted)
	if err != nil {
		return nil, err
	}
	privateKey, err := crypto.AesDecrypt(encryptedPrivateKey, masterKey, iv)
	if err != nil {
		return nil, err
	}

	if err := crypto.CheckPrivateKey(privateKey); err != nil {
		log.Error("open wallet error", err)
		os.Exit(1)
	}

	account, err := NewAccountWithPrivatekey(privateKey)
	if err != nil {
		return nil, err
	}

	rawdata, _ := HexStringToBytes(store.Data.ContractData)
	r := bytes.NewReader(rawdata)
	ct := new(contract.Contract)
	ct.Deserialize(r)

	return &WalletImpl{
		path:        path,
		iv:          iv,
		masterKey:   masterKey,
		account:     account,
		contract:    ct,
		WalletStore: store,
	}, nil
}

func RecoverWallet(path string, password []byte, privateKeyHex string) (*WalletImpl, error) {
	wallet, err := NewWallet(path, password, false)
	if err != nil {
		return nil, errors.New("create new wallet error")
	}
	privateKey, err := HexStringToBytes(privateKeyHex)
	if err != nil {
		return nil, err
	}
	err = wallet.CreateAccount(privateKey)
	if err != nil {
		return nil, err
	}

	return wallet, nil
}

func (w *WalletImpl) CreateAccount(privateKey []byte) error {
	var account *Account
	var err error
	if privateKey == nil {
		account, err = NewAccount()
	} else {
		account, err = NewAccountWithPrivatekey(privateKey)
	}
	if err != nil {
		return err
	}
	encryptedPrivateKey, err := crypto.AesEncrypt(account.PrivateKey, w.masterKey, w.iv)
	if err != nil {
		return err
	}
	contract, err := contract.CreateSignatureContract(account.PubKey())
	if err != nil {
		return err
	}
	err = w.SaveAccountData(account.ProgramHash.ToArray(), encryptedPrivateKey, contract.ToArray())
	if err != nil {
		return err
	}
	w.account = account

	return nil
}

func (w *WalletImpl) GetDefaultAccount() (*Account, error) {
	if w.account == nil {
		return nil, errors.New("account error")
	}
	return w.account, nil
}

func (w *WalletImpl) GetAccount(pubKey *crypto.PubKey) (*Account, error) {
	redeemHash, err := contract.CreateRedeemHash(pubKey)
	if err != nil {
		return nil, fmt.Errorf("%v\n%s", err, "[Account] GetAccount redeemhash generated failed")
	}

	if redeemHash != w.account.ProgramHash {
		return nil, errors.New("invalid account")
	}

	return w.account, nil
}

func (w *WalletImpl) Sign(context *contract.ContractContext) error {
	var err error
	contract, err := w.GetContract()
	if err != nil {
		return errors.New("no available contract in wallet")
	}
	account, err := w.GetDefaultAccount()
	if err != nil {
		return errors.New("no available account in wallet")
	}

	signature, err := signature.SignBySigner(context.Data, account)
	if err != nil {
		return err
	}
	err = context.AddContract(contract, account.PublicKey, signature)
	if err != nil {
		return err
	}

	return nil
}

func verifyPasswordKey(passwordKey []byte, passwordHash []byte) bool {
	keyHash := sha256.Sum256(passwordKey)
	if !IsEqualBytes(passwordHash, keyHash[:]) {
		fmt.Println("error: password wrong")
		return false
	}

	return true
}

func (w *WalletImpl) ChangePassword(oldPassword []byte, newPassword []byte) bool {
	// check original password
	oldPasswordKey := crypto.ToAesKey(oldPassword)
	passwordKeyHash, err := HexStringToBytes(w.Data.PasswordHash)
	if err != nil {
		return false
	}
	if ok := verifyPasswordKey(oldPasswordKey, passwordKeyHash); !ok {
		return false
	}

	// encrypt master key with new password
	newPasswordKey := crypto.ToAesKey(newPassword)
	newPasswordHash := sha256.Sum256(newPasswordKey)
	newMasterKey, err := crypto.AesEncrypt(w.masterKey, newPasswordKey, w.iv)
	if err != nil {
		fmt.Println("error: set new password failed")
		return false
	}

	// update wallet file
	err = w.SaveBasicData([]byte(WalletStoreVersion), w.iv, newMasterKey, newPasswordHash[:])
	if err != nil {
		return false
	}

	return true
}

func (w *WalletImpl) GetContract() (*contract.Contract, error) {
	if w.contract == nil {
		return nil, errors.New("contract error")
	}

	return w.contract, nil
}

//func (w *WalletImpl) GetUnspent() (map[Uint256][]*transaction.UTXOUnspent, error) {
//	//TODO fix it
//	return nil, nil
//}

func GetWallet() Wallet {
	if !FileExisted(WalletFileName) {
		log.Errorf(fmt.Sprintf("No %s detected, please create a wallet by using command line.", WalletFileName))
		os.Exit(1)
	}
	passwd, err := password.GetAccountPassword()
	if err != nil {
		log.Error("Get password error.")
		os.Exit(1)
	}
	c, err := OpenWallet(WalletFileName, passwd)
	if err != nil {
		log.Error(err)
		return nil
	}
	return c
}
