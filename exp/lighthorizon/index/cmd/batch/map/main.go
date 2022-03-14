package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stellar/go/exp/lighthorizon/index"
	"github.com/stellar/go/historyarchive"
	"github.com/stellar/go/ingest"
	"github.com/stellar/go/network"
	"github.com/stellar/go/support/log"
	"github.com/stellar/go/xdr"
	"golang.org/x/sync/errgroup"
)

var (
	// Should we use runtime.NumCPU() for a reasonable default?
	parallel = uint32(20)
)

func main() {
	log.SetLevel(log.InfoLevel)
	startTime := time.Now()

	jobIndexString := os.Getenv("AWS_BATCH_JOB_ARRAY_INDEX")
	if jobIndexString == "" {
		panic("AWS_BATCH_JOB_ARRAY_INDEX env required")
	}

	jobIndex, err := strconv.ParseUint(jobIndexString, 10, 64)
	if err != nil {
		panic(err)
	}

	firstCheckpointString := os.Getenv("FIRST_CHECKPOINT")
	firstCheckpoint, err := strconv.ParseUint(firstCheckpointString, 10, 64)
	if err != nil {
		panic(err)
	}

	batchSizeString := os.Getenv("BATCH_SIZE")
	batchSize, err := strconv.ParseUint(batchSizeString, 10, 64)
	if err != nil {
		panic(err)
	}

	startCheckpoint := uint32(firstCheckpoint + batchSize*jobIndex)
	endCheckpoint := startCheckpoint + uint32(batchSize) - 1

	indexStore, err := index.NewS3Store(
		&aws.Config{Region: aws.String("us-east-1")},
		fmt.Sprintf("job_%d", jobIndex),
		parallel,
	)
	if err != nil {
		panic(err)
	}

	historyArchive, err := historyarchive.Connect(
		"s3://history.stellar.org/prd/core-live/core_live_001",
		historyarchive.ConnectOptions{
			NetworkPassphrase: network.PublicNetworkPassphrase,
			S3Region:          "eu-west-1",
			UnsignedRequests:  true,
		},
	)
	if err != nil {
		panic(err)
	}

	all := endCheckpoint - startCheckpoint

	ctx := context.Background()
	wg, _ := errgroup.WithContext(ctx)

	ch := make(chan uint32, parallel)

	go func() {
		for i := startCheckpoint; i <= endCheckpoint; i++ {
			ch <- i
		}
		close(ch)
	}()

	processed := uint64(0)
	for i := uint32(0); i < parallel; i++ {
		wg.Go(func() error {
			for checkpoint := range ch {

				startLedger := checkpoint * 64
				if startLedger == 0 {
					startLedger = 1
				}
				endLedger := checkpoint*64 + 64 - 1

				log.Info("Processing checkpoint ", checkpoint, " ledgers ", startLedger, endLedger)

				ledgers, err := historyArchive.GetLedgers(startLedger, endLedger)
				if err != nil {
					log.WithField("error", err).Error("error getting ledgers")
					ch <- checkpoint
					continue
				}

				for i := startLedger; i <= endLedger; i++ {
					ledger, ok := ledgers[i]
					if !ok {
						return fmt.Errorf("no ledger %d", i)
					}

					resultMeta := make([]xdr.TransactionResultMeta, len(ledger.TransactionResult.TxResultSet.Results))
					for i, result := range ledger.TransactionResult.TxResultSet.Results {
						resultMeta[i].Result = result
					}

					closeMeta := xdr.LedgerCloseMeta{
						V0: &xdr.LedgerCloseMetaV0{
							LedgerHeader: ledger.Header,
							TxSet:        ledger.Transaction.TxSet,
							TxProcessing: resultMeta,
						},
					}

					reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(network.PublicNetworkPassphrase, closeMeta)
					if err != nil {
						return err
					}

					for {
						tx, err := reader.Read()
						if err != nil {
							if err == io.EOF {
								break
							}
							return err
						}

						allParticipants, err := participantsForOperations(tx, false)
						if err != nil {
							return err
						}

						err = indexStore.AddParticipantsToIndexesNoBackend(checkpoint, "all_all", allParticipants)
						if err != nil {
							return err
						}

						paymentsParticipants, err := participantsForOperations(tx, true)
						if err != nil {
							return err
						}

						err = indexStore.AddParticipantsToIndexesNoBackend(checkpoint, "all_payments", paymentsParticipants)
						if err != nil {
							return err
						}

						if tx.Result.Successful() {
							allParticipants, err := participantsForOperations(tx, false)
							if err != nil {
								return err
							}

							err = indexStore.AddParticipantsToIndexesNoBackend(checkpoint, "successful_all", allParticipants)
							if err != nil {
								return err
							}

							paymentsParticipants, err := participantsForOperations(tx, true)
							if err != nil {
								return err
							}

							err = indexStore.AddParticipantsToIndexesNoBackend(checkpoint, "successful_payments", paymentsParticipants)
							if err != nil {
								return err
							}
						}
					}
				}

				nprocessed := atomic.AddUint64(&processed, 1)

				if nprocessed%100 == 0 {
					log.Infof(
						"Reading checkpoints... - %.2f%% - elapsed: %s, remaining: %s",
						(float64(nprocessed)/float64(all))*100,
						time.Since(startTime).Round(1*time.Second),
						(time.Duration(int64(time.Since(startTime))*int64(all)/int64(nprocessed)) - time.Since(startTime)).Round(1*time.Second),
					)
				}
			}
			return nil
		})
	}

	if err := wg.Wait(); err != nil {
		panic(err)
	}
	log.Infof("Uploading accounts")
	if err := indexStore.FlushAccounts(); err != nil {
		panic(err)
	}
	log.Infof("Uploading indexes")
	if err := indexStore.Flush(); err != nil {
		panic(err)
	}
}

func participantsForOperations(transaction ingest.LedgerTransaction, onlyPayments bool) ([]string, error) {
	var participants []string

	for opindex, operation := range transaction.Envelope.Operations() {
		opSource := operation.SourceAccount
		if opSource == nil {
			txSource := transaction.Envelope.SourceAccount()
			opSource = &txSource
		}

		switch operation.Body.Type {
		case xdr.OperationTypeCreateAccount,
			xdr.OperationTypePayment,
			xdr.OperationTypePathPaymentStrictReceive,
			xdr.OperationTypePathPaymentStrictSend,
			xdr.OperationTypeAccountMerge:
			participants = append(participants, opSource.Address())
		default:
			if onlyPayments {
				continue
			}
			participants = append(participants, opSource.Address())
		}

		switch operation.Body.Type {
		case xdr.OperationTypeCreateAccount:
			participants = append(participants, operation.Body.MustCreateAccountOp().Destination.Address())
		case xdr.OperationTypePayment:
			participants = append(participants, operation.Body.MustPaymentOp().Destination.ToAccountId().Address())
		case xdr.OperationTypePathPaymentStrictReceive:
			participants = append(participants, operation.Body.MustPathPaymentStrictReceiveOp().Destination.ToAccountId().Address())
		case xdr.OperationTypePathPaymentStrictSend:
			participants = append(participants, operation.Body.MustPathPaymentStrictSendOp().Destination.ToAccountId().Address())
		case xdr.OperationTypeManageBuyOffer:
			// the only direct participant is the source_account
		case xdr.OperationTypeManageSellOffer:
			// the only direct participant is the source_account
		case xdr.OperationTypeCreatePassiveSellOffer:
			// the only direct participant is the source_account
		case xdr.OperationTypeSetOptions:
			// the only direct participant is the source_account
		case xdr.OperationTypeChangeTrust:
			// the only direct participant is the source_account
		case xdr.OperationTypeAllowTrust:
			participants = append(participants, operation.Body.MustAllowTrustOp().Trustor.Address())
		case xdr.OperationTypeAccountMerge:
			participants = append(participants, operation.Body.MustDestination().ToAccountId().Address())
		case xdr.OperationTypeInflation:
			// the only direct participant is the source_account
		case xdr.OperationTypeManageData:
			// the only direct participant is the source_account
		case xdr.OperationTypeBumpSequence:
			// the only direct participant is the source_account
		case xdr.OperationTypeCreateClaimableBalance:
			for _, c := range operation.Body.MustCreateClaimableBalanceOp().Claimants {
				participants = append(participants, c.MustV0().Destination.Address())
			}
		case xdr.OperationTypeClaimClaimableBalance:
			// the only direct participant is the source_account
		case xdr.OperationTypeBeginSponsoringFutureReserves:
			participants = append(participants, operation.Body.MustBeginSponsoringFutureReservesOp().SponsoredId.Address())
		case xdr.OperationTypeEndSponsoringFutureReserves:
			// Failed transactions may not have a compliant sandwich structure
			// we can rely on (e.g. invalid nesting or a being operation with the wrong sponsoree ID)
			// and thus we bail out since we could return incorrect information.
			if transaction.Result.Successful() {
				sponsoree := transaction.Envelope.SourceAccount().ToAccountId().Address()
				if operation.SourceAccount != nil {
					sponsoree = operation.SourceAccount.Address()
				}
				operations := transaction.Envelope.Operations()
				for i := int(opindex) - 1; i >= 0; i-- {
					if beginOp, ok := operations[i].Body.GetBeginSponsoringFutureReservesOp(); ok &&
						beginOp.SponsoredId.Address() == sponsoree {
						participants = append(participants, beginOp.SponsoredId.Address())
					}
				}
			}
		case xdr.OperationTypeRevokeSponsorship:
			op := operation.Body.MustRevokeSponsorshipOp()
			switch op.Type {
			case xdr.RevokeSponsorshipTypeRevokeSponsorshipLedgerEntry:
				participants = append(participants, getLedgerKeyParticipants(*op.LedgerKey)...)
			case xdr.RevokeSponsorshipTypeRevokeSponsorshipSigner:
				participants = append(participants, op.Signer.AccountId.Address())
				// We don't add signer as a participant because a signer can be arbitrary account.
				// This can spam successful operations history of any account.
			}
		case xdr.OperationTypeClawback:
			op := operation.Body.MustClawbackOp()
			participants = append(participants, op.From.ToAccountId().Address())
		case xdr.OperationTypeClawbackClaimableBalance:
			// the only direct participant is the source_account
		case xdr.OperationTypeSetTrustLineFlags:
			op := operation.Body.MustSetTrustLineFlagsOp()
			participants = append(participants, op.Trustor.Address())
		case xdr.OperationTypeLiquidityPoolDeposit:
			// the only direct participant is the source_account
		case xdr.OperationTypeLiquidityPoolWithdraw:
			// the only direct participant is the source_account
		default:
			return nil, fmt.Errorf("unknown operation type: %s", operation.Body.Type)
		}

		// Requires meta
		// sponsor, err := operation.getSponsor()
		// if err != nil {
		// 	return nil, err
		// }
		// if sponsor != nil {
		// 	otherParticipants = append(otherParticipants, *sponsor)
		// }
	}

	return participants, nil
}

func getLedgerKeyParticipants(ledgerKey xdr.LedgerKey) []string {
	var result []string
	switch ledgerKey.Type {
	case xdr.LedgerEntryTypeAccount:
		result = append(result, ledgerKey.Account.AccountId.Address())
	case xdr.LedgerEntryTypeClaimableBalance:
		// nothing to do
	case xdr.LedgerEntryTypeData:
		result = append(result, ledgerKey.Data.AccountId.Address())
	case xdr.LedgerEntryTypeOffer:
		result = append(result, ledgerKey.Offer.SellerId.Address())
	case xdr.LedgerEntryTypeTrustline:
		result = append(result, ledgerKey.TrustLine.AccountId.Address())
	}
	return result
}