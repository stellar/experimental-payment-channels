package txbuild

import (
	"time"

	"github.com/stellar/go/amount"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/txnbuild"
)

type CloseParams struct {
	ObservationPeriodTime      time.Duration
	ObservationPeriodLedgerGap int64
	InitiatorSigner            *keypair.FromAddress
	ResponderSigner            *keypair.FromAddress
	InitiatorEscrow            *keypair.FromAddress
	ResponderEscrow            *keypair.FromAddress
	StartSequence              int64
	IterationNumber            int64
	AmountToInitiator          int64
	AmountToResponder          int64
}

func Close(p CloseParams) (*txnbuild.Transaction, error) {
	tp := txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{
			AccountID: p.InitiatorEscrow.Address(),
			Sequence:  startSequenceOfIteration(p.StartSequence, p.IterationNumber) + 1, // Close is the second transaction in an iteration's transaction set.
		},
		BaseFee:              txnbuild.MinBaseFee,
		Timebounds:           txnbuild.NewTimeout(300),
		MinSequenceAge:       int64(p.ObservationPeriodTime.Seconds()),
		MinSequenceLedgerGap: p.ObservationPeriodLedgerGap,
		Operations: []txnbuild.Operation{
			&txnbuild.SetOptions{
				SourceAccount:   p.InitiatorEscrow.Address(),
				MasterWeight:    txnbuild.NewThreshold(0),
				LowThreshold:    txnbuild.NewThreshold(1),
				MediumThreshold: txnbuild.NewThreshold(1),
				HighThreshold:   txnbuild.NewThreshold(1),
				Signer:          &txnbuild.Signer{Address: p.ResponderSigner.Address(), Weight: 0},
			},
			&txnbuild.SetOptions{
				SourceAccount:   p.ResponderEscrow.Address(),
				MasterWeight:    txnbuild.NewThreshold(0),
				LowThreshold:    txnbuild.NewThreshold(1),
				MediumThreshold: txnbuild.NewThreshold(1),
				HighThreshold:   txnbuild.NewThreshold(1),
				Signer:          &txnbuild.Signer{Address: p.InitiatorSigner.Address(), Weight: 0},
			},
		},
	}
	if p.AmountToInitiator != 0 {
		tp.Operations = append(tp.Operations, &txnbuild.Payment{
			SourceAccount: p.ResponderEscrow.Address(),
			Destination:   p.InitiatorEscrow.Address(),
			Asset:         txnbuild.NativeAsset{},
			Amount:        amount.StringFromInt64(p.AmountToInitiator),
		})
	}
	if p.AmountToResponder != 0 {
		tp.Operations = append(tp.Operations, &txnbuild.Payment{
			SourceAccount: p.InitiatorEscrow.Address(),
			Destination:   p.ResponderEscrow.Address(),
			Asset:         txnbuild.NativeAsset{},
			Amount:        amount.StringFromInt64(p.AmountToResponder),
		})
	}
	tx, err := txnbuild.NewTransaction(tp)
	if err != nil {
		return nil, err
	}
	return tx, nil
}