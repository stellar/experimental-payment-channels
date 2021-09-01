package txbuildtest

import (
	"fmt"

	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// BuildResult returns a result XDR base64 encoded that is successful or not
// based on the input parameter.
func BuildResultXDR(success bool) (string, error) {
	var code xdr.TransactionResultCode
	if success {
		code = xdr.TransactionResultCodeTxSuccess
	} else {
		code = xdr.TransactionResultCodeTxFailed
	}
	tr := xdr.TransactionResult{
		FeeCharged: 123,
		Result: xdr.TransactionResultResult{
			Code:    code,
			Results: &[]xdr.OperationResult{},
		},
	}
	trXDR, err := xdr.MarshalBase64(tr)
	if err != nil {
		return "", fmt.Errorf("encoding transaction result to base64 xdr: %w", err)
	}
	return trXDR, nil
}

// BuildResultMetaXDR returns a result meta XDR base64 encoded that contains
// the input ledger entry changes. Only creates one operation meta for
// simiplicity.
func BuildResultMetaXDR(ledgerEntryResults []xdr.LedgerEntryData) (string, error) {
	tm := xdr.TransactionMeta{
		V: 2,
		V2: &xdr.TransactionMetaV2{
			Operations: []xdr.OperationMeta{
				{},
			},
		},
	}

	for _, result := range ledgerEntryResults {
		tm.V2.Operations[0].Changes = append(tm.V2.Operations[0].Changes, xdr.LedgerEntryChange{
			Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
			Updated: &xdr.LedgerEntry{
				Data: result,
			},
		})
	}

	tmXDR, err := xdr.MarshalBase64(tm)
	if err != nil {
		return "", fmt.Errorf("encoding transaction meta to base64 xdr: %w", err)
	}
	return tmXDR, nil
}

type FormationResultMetaParams struct {
	InitiatorSigner string
	ResponderSigner string
	InitiatorEscrow string
	ResponderEscrow string
	StartSequence   int64
	Asset           txnbuild.Asset
}

func BuildFormationResultMetaXDR(params FormationResultMetaParams) (string, error) {
	led := []xdr.LedgerEntryData{
		{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId: xdr.MustAddress(params.InitiatorEscrow),
				SeqNum:    xdr.SequenceNumber(params.StartSequence),
				Signers: []xdr.Signer{
					{
						Key:    xdr.MustSigner(params.InitiatorSigner),
						Weight: 1,
					},
					{
						Key:    xdr.MustSigner(params.ResponderSigner),
						Weight: 1,
					},
				},
				Thresholds: xdr.Thresholds{0, 2, 2, 2},
			},
		},
		{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId: xdr.MustAddress(params.ResponderEscrow),
				SeqNum:    xdr.SequenceNumber(1),
				Signers: []xdr.Signer{
					{
						Key:    xdr.MustSigner(params.InitiatorSigner),
						Weight: 1,
					},
					{
						Key:    xdr.MustSigner(params.ResponderSigner),
						Weight: 1,
					},
				},
				Thresholds: xdr.Thresholds{0, 2, 2, 2},
			},
		},
	}

	if !params.Asset.IsNative() {
		led = append(led, []xdr.LedgerEntryData{
			{
				Type: xdr.LedgerEntryTypeTrustline,
				TrustLine: &xdr.TrustLineEntry{
					AccountId: xdr.MustAddress(params.InitiatorEscrow),
					Balance:   0,
					Asset:     xdr.MustNewCreditAsset(params.Asset.GetCode(), params.Asset.GetIssuer()),
					Flags:     xdr.Uint32(xdr.TrustLineFlagsAuthorizedFlag),
				},
			},
			{
				Type: xdr.LedgerEntryTypeTrustline,
				TrustLine: &xdr.TrustLineEntry{
					AccountId: xdr.MustAddress(params.ResponderEscrow),
					Balance:   0,
					Asset:     xdr.MustNewCreditAsset(params.Asset.GetCode(), params.Asset.GetIssuer()),
					Flags:     xdr.Uint32(xdr.TrustLineFlagsAuthorizedFlag),
				},
			},
		}...)
	}

	return BuildResultMetaXDR(led)
}
