package state

import (
	"fmt"

	"github.com/stellar/experimental-payment-channels/sdk/txbuild"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// The steps for a channel close are as follows:
// 1. A submits latest declaration tx
// 2. A calls ProposeClose to propose an immediate close by resigning the
//    current close tx
// 3. B calls ConfirmClose to sign and store result
// 4. A calls ConfirmClose to store result
// 5. A or B submit the new close tx
// 6. If A or B declines or is not responsive at any step, A or B may submit the
//    original close tx after the observation period.

func (c *Channel) closeTxs(oad OpenAgreementDetails, d CloseAgreementDetails) (txDeclHash TransactionHash, txDecl *txnbuild.Transaction, txCloseHash TransactionHash, txClose *txnbuild.Transaction, err error) {
	txClose, err = txbuild.Close(txbuild.CloseParams{
		ObservationPeriodTime:      d.ObservationPeriodTime,
		ObservationPeriodLedgerGap: d.ObservationPeriodLedgerGap,
		InitiatorSigner:            c.initiatorSigner(),
		ResponderSigner:            c.responderSigner(),
		InitiatorEscrow:            c.initiatorEscrowAccount().Address,
		ResponderEscrow:            c.responderEscrowAccount().Address,
		StartSequence:              oad.StartingSequence,
		IterationNumber:            d.IterationNumber,
		AmountToInitiator:          amountToInitiator(d.Balance),
		AmountToResponder:          amountToResponder(d.Balance),
		Asset:                      oad.Asset.Asset(),
	})
	if err != nil {
		return TransactionHash{}, nil, TransactionHash{}, nil, err
	}
	txCloseHash, err = txClose.Hash(c.networkPassphrase)
	if err != nil {
		return TransactionHash{}, nil, TransactionHash{}, nil, err
	}
	txDecl, err = txbuild.Declaration(txbuild.DeclarationParams{
		InitiatorEscrow:         c.initiatorEscrowAccount().Address,
		StartSequence:           oad.StartingSequence,
		IterationNumber:         d.IterationNumber,
		IterationNumberExecuted: 0,
		ConfirmingSigner:        d.ConfirmingSigner,
		CloseTxHash:             txCloseHash,
	})
	if err != nil {
		return TransactionHash{}, nil, TransactionHash{}, nil, err
	}
	txDeclHash, err = txDecl.Hash(c.networkPassphrase)
	if err != nil {
		return TransactionHash{}, nil, TransactionHash{}, nil, err
	}
	return txDeclHash, txDecl, txCloseHash, txClose, nil
}

// CloseTxs builds the declaration and close transactions used for closing the
// channel using the latest close agreement. The transactions are signed and
// ready to submit.
func (c *Channel) CloseTxs() (declTx *txnbuild.Transaction, closeTx *txnbuild.Transaction, err error) {
	ca := c.latestAuthorizedCloseAgreement
	declTxHash, declTx, closeTxHash, closeTx, err := c.closeTxs(c.openAgreement.Details, ca.Details)
	if err != nil {
		return nil, nil, fmt.Errorf("building declaration and close txs for latest close agreement: %w", err)
	}

	// Check that the transactions built match the transaction hashes in the
	// close agreement.
	if ca.TransactionHashes.Declaration != declTxHash {
		// TODO
	}
	if ca.TransactionHashes.Close != closeTxHash {
		// TODO
	}

	// Add the declaration signatures to the declaration tx.
	declTx, _ = declTx.AddSignatureDecorated(xdr.NewDecoratedSignature(ca.ProposerSignatures.Declaration, ca.Details.ProposingSigner.Hint()))
	declTx, _ = declTx.AddSignatureDecorated(xdr.NewDecoratedSignature(ca.ConfirmerSignatures.Declaration, ca.Details.ConfirmingSigner.Hint()))

	// Add the close signature provided by the confirming signer that is
	// required to be an extra signer on the declaration tx to the formation tx.
	declTx, _ = declTx.AddSignatureDecorated(xdr.NewDecoratedSignatureForPayload(ca.ConfirmerSignatures.Close, ca.Details.ConfirmingSigner.Hint(), closeTxHash[:]))

	// Add the close signatures to the close tx.
	closeTx, _ = closeTx.AddSignatureDecorated(xdr.NewDecoratedSignature(ca.ProposerSignatures.Close, ca.Details.ProposingSigner.Hint()))
	closeTx, _ = closeTx.AddSignatureDecorated(xdr.NewDecoratedSignature(ca.ConfirmerSignatures.Close, ca.Details.ConfirmingSigner.Hint()))

	return
}

// ProposeClose proposes that the latest authorized close agreement be submitted
// without waiting the observation period. This should be used when participants
// are in agreement on the final close state, but would like to submit earlier
// than the original observation time.
func (c *Channel) ProposeClose() (CloseAgreement, error) {
	// If an unfinished unauthorized agreement exists, error.
	if !c.latestUnauthorizedCloseAgreement.isEmpty() {
		return CloseAgreement{}, fmt.Errorf("cannot propose coordinated close while an unfinished payment exists")
	}

	// If the channel is not open yet, error.
	if c.latestAuthorizedCloseAgreement.isEmpty() || !c.openExecutedAndValidated {
		return CloseAgreement{}, fmt.Errorf("cannot propose a coordinated close before channel is opened")
	}

	d := c.latestAuthorizedCloseAgreement.Details
	d.ObservationPeriodTime = 0
	d.ObservationPeriodLedgerGap = 0
	d.ProposingSigner = c.localSigner.FromAddress()
	d.ConfirmingSigner = c.remoteSigner

	txDeclHash, txDecl, txCloseHash, txClose, err := c.closeTxs(c.openAgreement.Details, d)
	if err != nil {
		return CloseAgreement{}, fmt.Errorf("making declaration and close transactions: %w", err)
	}
	sigs, err := signCloseAgreementTxs(txDecl, txClose, c.networkPassphrase, c.localSigner)
	if err != nil {
		return CloseAgreement{}, fmt.Errorf("signing open agreement with local: %w", err)
	}

	// Store the close agreement while participants iterate on signatures.
	c.latestUnauthorizedCloseAgreement = CloseAgreement{
		Details: d,
		TransactionHashes: CloseAgreementTransactionHashes{
			Declaration: txDeclHash,
			Close:       txCloseHash,
		},
		ProposerSignatures: sigs,
	}
	return c.latestUnauthorizedCloseAgreement, nil
}

func (c *Channel) validateClose(ca CloseAgreement) error {
	// If the channel is not open yet, error.
	if c.latestAuthorizedCloseAgreement.isEmpty() || !c.openExecutedAndValidated {
		return fmt.Errorf("cannot confirm a coordinated close before channel is opened")
	}
	if ca.Details.IterationNumber != c.latestAuthorizedCloseAgreement.Details.IterationNumber {
		return fmt.Errorf("close agreement iteration number does not match saved latest authorized close agreement")
	}
	if ca.Details.Balance != c.latestAuthorizedCloseAgreement.Details.Balance {
		return fmt.Errorf("close agreement balance does not match saved latest authorized close agreement")
	}
	if ca.Details.ObservationPeriodTime != 0 {
		return fmt.Errorf("close agreement observation period time is not zero")
	}
	if ca.Details.ObservationPeriodLedgerGap != 0 {
		return fmt.Errorf("close agreement observation period ledger gap is not zero")
	}
	if !ca.Details.ConfirmingSigner.Equal(c.localSigner.FromAddress()) && !ca.Details.ConfirmingSigner.Equal(c.remoteSigner) {
		return fmt.Errorf("close agreement confirmer does not match a local or remote signer, got: %s", ca.Details.ConfirmingSigner.Address())
	}
	return nil
}

// ConfirmClose agrees to a close agreement to be submitted without waiting the
// observation period. The agreement will always be accepted if it is identical
// to the latest authorized close agreement, and it is signed by the participant
// proposing the close.
func (c *Channel) ConfirmClose(ca CloseAgreement) (closeAgreement CloseAgreement, err error) {
	err = c.validateClose(ca)
	if err != nil {
		return CloseAgreement{}, fmt.Errorf("validating close agreement: %w", err)
	}

	txDeclHash, txDecl, txCloseHash, txClose, err := c.closeTxs(c.openAgreement.Details, ca.Details)
	if err != nil {
		return CloseAgreement{}, fmt.Errorf("making close transactions: %w", err)
	}

	// Check that the transactions built match the transaction hashes in the
	// close agreement.
	if ca.TransactionHashes.Declaration != txDeclHash {
		// TODO
	}
	if ca.TransactionHashes.Close != txCloseHash {
		// TODO
	}

	// If remote has not signed the txs, error as is invalid.
	remoteSigs := ca.SignaturesFor(c.remoteSigner)
	if remoteSigs == nil {
		return CloseAgreement{}, fmt.Errorf("remote is not a signer")
	}
	err = remoteSigs.Verify(txDecl, txClose, c.networkPassphrase, c.remoteSigner)
	if err != nil {
		return CloseAgreement{}, fmt.Errorf("not signed by remote: %w", err)
	}

	// If local has not signed close, check that the payment is not to the proposer, then sign.
	localSigs := ca.SignaturesFor(c.localSigner.FromAddress())
	if localSigs == nil {
		return CloseAgreement{}, fmt.Errorf("local is not a signer")
	}
	err = localSigs.Verify(txDecl, txClose, c.networkPassphrase, c.localSigner.FromAddress())
	if err != nil {
		// If the local is not the confirmer, do not sign, because being the
		// proposer they should have signed earlier.
		if !ca.Details.ConfirmingSigner.Equal(c.localSigner.FromAddress()) {
			return CloseAgreement{}, fmt.Errorf("not signed by local: %w", err)
		}
		ca.ConfirmerSignatures, err = signCloseAgreementTxs(txDecl, txClose, c.networkPassphrase, c.localSigner)
		if err != nil {
			return CloseAgreement{}, fmt.Errorf("local signing: %w", err)
		}
	}

	// The new close agreement is valid and authorized, store and promote it.
	c.latestAuthorizedCloseAgreement = ca
	c.latestUnauthorizedCloseAgreement = CloseAgreement{}
	return c.latestAuthorizedCloseAgreement, nil
}
