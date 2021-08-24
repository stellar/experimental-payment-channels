package state

import (
	"fmt"

	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// IngestTx accepts any transaction that has been seen as successful or
// unsuccessful on the network. The function updates the internal state of the
// channel if the transaction relates to the channel.
//
// TODO: Signal when the state of the channel has changed to closed or closing.
// TODO: Accept the xdr.TransactionResult and xdr.TransactionMeta so code can
// determine if successful or not, and understand changes in the ledger as a
// result.
func (c *Channel) IngestTx(tx *txnbuild.Transaction, resultMetaXDR string) error {
	// TODO: Use the transaction result to affect on success/failure.

	c.ingestTxToUpdateInitiatorEscrowAccountSequence(tx)

	err := c.ingestTxToUpdateUnauthorizedCloseAgreement(tx)
	if err != nil {
		return err
	}

	err = c.ingestTxMetaToUpdateBalances(resultMetaXDR)
	if err != nil {
		return err
	}

	err = c.ingestFormationTx(resultMetaXDR)
	if err != nil {
		return err
	}

	return nil
}

func (c *Channel) ingestFormationTx(resultMetaXDR string) error {
	// TODO - identify if this is a formation transaction, if not return

	// If not a valid resultMetaXDR string, return error.
	var txMeta xdr.TransactionMeta
	err := xdr.SafeUnmarshalBase64(resultMetaXDR, &txMeta)
	if err != nil {
		return fmt.Errorf("parsing the result meta xdr: %w", err)
	}

	// TODO - can check the extraSignatures fields?

	var initiatorEscrowAccountEntry, responderEscrowAccountEntry xdr.AccountEntry
	var initiatorEscrowTrustlineEntry, responderEscrowTrustlineEntry xdr.TrustLineEntry
	for _, o := range txMeta.V2.Operations {
		for _, change := range o.Changes {
			updated, ok := change.GetUpdated()
			if !ok {
				continue
			}

			// Grab any trustline updates in case the channel asset is non-native.
			trustline, ok := updated.Data.GetTrustLine()
			if ok {
				if trustline.AccountId.Address() == c.initiatorEscrowAccount().Address.Address() {
					initiatorEscrowTrustlineEntry = trustline
				} else if trustline.AccountId.Address() == c.responderEscrowAccount().Address.Address() {
					responderEscrowTrustlineEntry = trustline
				}
			}

			account, ok := updated.Data.GetAccount()
			if !ok {
				continue
			}
			if account.AccountId.Address() == c.initiatorEscrowAccount().Address.Address() {
				initiatorEscrowAccountEntry = account
			} else if account.AccountId.Address() == c.responderEscrowAccount().Address.Address() {
				responderEscrowAccountEntry = account
			}
		}
	}

	// Validate the initiator escrow account sequence number is correct.
	if int64(initiatorEscrowAccountEntry.SeqNum) != c.startingSequence {
		return fmt.Errorf("incorrect initiator escrow account sequence number found, found: %d want: %d",
			int64(initiatorEscrowAccountEntry.SeqNum), c.startingSequence)
	}

	escrowAccounts := [2]xdr.AccountEntry{initiatorEscrowAccountEntry, responderEscrowAccountEntry}
	for _, ea := range escrowAccounts {
		// Validate the escrow account thresholds are correct.
		if ea.Thresholds != xdr.Thresholds([4]byte{0, 2, 2, 2}) {
			return fmt.Errorf("incorrect initiator escrow account thresholds found")
		}

		// Validate the escrow account has the correct signers and signer weights.
		var initiatorSignerFound, responderSignerFound bool
		for _, signer := range ea.Signers {
			address, err := signer.Key.GetAddress()
			if err != nil {
				return fmt.Errorf("parsing formation transaction escrow account signer keys: %w", err)
			}

			if address == c.initiatorSigner().Address() && signer.Weight == xdr.Uint32(1) {
				initiatorSignerFound = true
			} else if address == c.responderSigner().Address() && signer.Weight == xdr.Uint32(1) {
				responderSignerFound = true
			}
		}
		if !initiatorSignerFound || !responderSignerFound || len(ea.Signers) != 2 {
			return fmt.Errorf("incorrect signer weights found")
		}
	}

	// Validate the trustlines are correct.
	if c.openAgreement.Details.Asset.IsNative() {
		var empty xdr.TrustLineEntry
		if initiatorEscrowTrustlineEntry != empty || responderEscrowTrustlineEntry != empty {
			return fmt.Errorf("extraneous trustline found for native asset channel")
		}
	} else {
		trustlineEntries := [2]xdr.TrustLineEntry{initiatorEscrowTrustlineEntry, responderEscrowTrustlineEntry}
		for _, te := range trustlineEntries {
			if string(c.openAgreement.Details.Asset) != te.Asset.StringCanonical() {
				return fmt.Errorf("incorrect trustline asset found for nonnative asset channel")
			}
		}
	}

	c.formationTxSuccess = true
	return nil
}

func (c *Channel) ingestTxToUpdateInitiatorEscrowAccountSequence(tx *txnbuild.Transaction) {
	// If the transaction's source account is not the initiator's escrow
	// account, return.
	if tx.SourceAccount().AccountID != c.initiatorEscrowAccount().Address.Address() {
		return
	}

	c.setInitiatorEscrowAccountSequence(tx.SourceAccount().Sequence)
}

// ingestTxToUpdateUnauthorizedCloseAgreement uses the signatures in the transaction to
// authorize an unauthorized close agreement if the channel has one.
// This process helps to give a participant who proposed an agreement the
// ability to close the channel if they did not receive the confirmers
// signatures for a close agreement when the agreement was being negotiated. If
// the transaction cannot be used to do this the function returns a nil error.
// If the transaction should be able to provide this data and cannot, the
// function errors.
func (c *Channel) ingestTxToUpdateUnauthorizedCloseAgreement(tx *txnbuild.Transaction) error {
	// If the transaction's source account is not the initiator's escrow
	// account, then the transaction is not a part of a close agreement.
	if tx.SourceAccount().AccountID != c.initiatorEscrowAccount().Address.Address() {
		return nil
	}

	ca := c.latestUnauthorizedCloseAgreement

	// If there is no unauthorized close agreement, then there's no need to try
	// and update it.
	if ca.isEmpty() {
		return nil
	}

	declTx, closeTx, err := c.closeTxs(c.openAgreement.Details, ca.Details)
	if err != nil {
		return fmt.Errorf("building txs for latest unauthorized close agreement: %w", err)
	}

	// Compare the hash of the tx with the hash of the declaration tx from the
	// latest unauthorized close agreement. If they match, then the tx is the
	// declaration tx.
	declTxHash, err := declTx.Hash(c.networkPassphrase)
	if err != nil {
		return fmt.Errorf("hashing latest unauthorized declaration tx: %w", err)
	}
	txHash, err := tx.Hash(c.networkPassphrase)
	if err != nil {
		return fmt.Errorf("hashing tx: %w", err)
	}
	if txHash != declTxHash {
		return nil
	}

	closeTxHash, err := closeTx.Hash(c.networkPassphrase)
	if err != nil {
		return fmt.Errorf("hashing latest unauthorized close tx: %w", err)
	}

	// Look for the signatures on the tx that are required to fully authorize
	// the unauthorized close agreement, then confirm the close agreement.
	for _, sig := range tx.Signatures() {
		err = c.remoteSigner.Verify(declTxHash[:], sig.Signature)
		if err == nil {
			ca.ConfirmerSignatures.Declaration = sig.Signature
			break
		}
	}
	for _, sig := range tx.Signatures() {
		err = c.remoteSigner.Verify(closeTxHash[:], sig.Signature)
		if err == nil {
			ca.ConfirmerSignatures.Close = sig.Signature
			break
		}
	}
	_, err = c.ConfirmPayment(ca)
	if err != nil {
		return fmt.Errorf("confirming the last unauthorized close: %w", err)
	}

	return nil
}

// ingestTxMetaToUpdateBalances uses the transaction result meta data
// from a transaction response to update local and remote escrow account
// balances.
func (c *Channel) ingestTxMetaToUpdateBalances(resultMetaXDR string) error {
	// If not a valid resultMetaXDR string, return.
	var txMeta xdr.TransactionMeta
	err := xdr.SafeUnmarshalBase64(resultMetaXDR, &txMeta)
	if err != nil {
		return fmt.Errorf("parsing the result meta xdr: %w", err)
	}

	channelAsset := c.openAgreement.Details.Asset

	// Find ledger changes for the escrow accounts' balances,
	// if any, and then update.
	for _, o := range txMeta.V2.Operations {
		for _, change := range o.Changes {
			updated, ok := change.GetUpdated()
			if !ok {
				continue
			}

			var ledgerEntryAddress string
			var ledgerEntryBalance int64

			if channelAsset.IsNative() {
				account, ok := updated.Data.GetAccount()
				if !ok {
					continue
				}
				ledgerEntryAddress = account.AccountId.Address()
				ledgerEntryBalance = int64(account.Balance)
			} else {
				tl, ok := updated.Data.GetTrustLine()
				if !ok {
					continue
				}
				if string(channelAsset) != tl.Asset.StringCanonical() {
					continue
				}
				ledgerEntryAddress = tl.AccountId.Address()
				ledgerEntryBalance = int64(tl.Balance)
			}

			switch ledgerEntryAddress {
			case c.localEscrowAccount.Address.Address():
				c.UpdateLocalEscrowAccountBalance(ledgerEntryBalance)
			case c.remoteEscrowAccount.Address.Address():
				c.UpdateRemoteEscrowAccountBalance(ledgerEntryBalance)
			}
		}
	}
	return nil
}
