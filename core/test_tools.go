package core

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"github.com/chain-lab/go-norn/common"
	"github.com/chain-lab/go-norn/crypto"
	log "github.com/sirupsen/logrus"
	karmem "karmem.org/golang"
	"time"
)

// buildTransaction 仅用于测试，也是一个构建交易的例子
func buildTransaction(key *ecdsa.PrivateKey) *common.Transaction {
	data := make([]byte, 32)
	rand.Read(data)
	timestamp := time.Now().UnixMilli()

	txBody := common.TransactionBody{
		Data:      data,
		Timestamp: timestamp,
		Expire:    timestamp + 3000,
	}

	txBody.Public = [33]byte(crypto.PublicKey2Bytes(&key.PublicKey))
	txBody.Address = crypto.PublicKeyBytes2Address(txBody.Public)

	writer := karmem.NewWriter(1024)
	txBody.WriteAsRoot(writer)
	txBodyBytes := writer.Bytes()

	hash := sha256.New()
	hash.Write(txBodyBytes)
	txHashBytes := hash.Sum(nil)
	txSignatureBytes, err := ecdsa.SignASN1(rand.Reader, key, txHashBytes)

	if err != nil {
		log.WithField("error", err).Errorln("Sign transaction failed.")
		return nil
	}

	txBody.Hash = [32]byte(txHashBytes)
	txBody.Signature = txSignatureBytes
	tx := common.Transaction{
		Body: txBody,
	}

	return &tx
}
