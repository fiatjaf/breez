package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/breez/breez/data"
	"github.com/fiatjaf/go-lnurl"
	"github.com/lightningnetwork/lnd/lnrpc"
)

type lnurlPayTempData struct {
	paymentHash  string
	lnurl        string
	callback     *url.URL
	metadata     string
	metadataHash string
	repeatable   bool
	success      *lnurl.SuccessAction
}

func (a *Service) HandleLNURL(encodedLnurl string) (*data.LNUrlResponse, error) {
	a.log.Infof("HandleLNURL %v", encodedLnurl)
	iparams, err := lnurl.HandleLNURL(encodedLnurl)
	if err != nil {
		return nil, err
	}

	switch params := iparams.(type) {
	case lnurl.LNURLWithdrawResponse:
		a.lnurlWithdrawing.callback = params.CallbackURL
		a.lnurlWithdrawing.k1 = params.K1
		a.log.Infof("lnurl-withdraw response: %v", params)
		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_Withdraw{
				&data.LNUrlWithdraw{
					MinAmount: int64(math.Ceil(
						float64(params.MinWithdrawable) / 1000,
					)),
					MaxAmount: int64(math.Floor(
						float64(params.MaxWithdrawable) / 1000,
					)),
					DefaultDescription: params.DefaultDescription,
				},
			},
		}, nil
	case lnurl.LNURLChannelResponse:
		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_Channel{
				Channel: &data.LNURLChannel{
					K1:       params.K1,
					Callback: params.Callback,
					Uri:      params.URI,
				},
			},
		}, nil
	case lnurl.LNURLPayResponse1:
		_metadataHash := sha256.Sum256([]byte(params.EncodedMetadata))
		metadataHash := hex.EncodeToString(_metadataHash[:])

		a.mu.Lock()
		// save this for FinishLNURLPay
		a.lnurlPaying = metadataHash

		// and save this for storing later at the payments db
		lnurlText, _ := lnurl.FindLNURLInText(encodedLnurl)
		a.lnurlPayTempCache[metadataHash] = lnurlPayTempData{
			lnurl:        lnurlText,
			callback:     params.CallbackURL,
			metadata:     params.EncodedMetadata,
			metadataHash: metadataHash,
		}
		a.mu.Unlock()

		// queue cleanup of that cache entry
		go func() {
			time.Sleep(10 * time.Minute)
			a.mu.Lock()
			delete(a.lnurlPayTempCache, metadataHash)
			a.mu.Unlock()
		}()

		invoiceMemo := data.InvoiceMemo{
			IsLnurlPay:    true,
			PayeeName:     params.CallbackURL.Host,
			PayeeImageURL: params.Metadata.ImageDataURI(),
			Amount:        params.MinSendable / 1000,
			MinSendable:   params.MinSendable / 1000,
			MaxSendable:   params.MaxSendable / 1000,
			Description:   params.Metadata.Description(),
		}

		if params.MinSendable == params.MaxSendable {
			invoiceMemo.MinSendable = 0
			invoiceMemo.MaxSendable = 0
		}

		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_Pay{
				Pay: &invoiceMemo,
			},
		}, nil
	default:
		return nil, errors.New("Unsupported LNUrl")
	}
}

func (a *Service) FinishLNURLWithdraw(bolt11 string) error {
	callback := a.lnurlWithdrawing.callback
	k1 := a.lnurlWithdrawing.k1

	qs := callback.Query()
	qs.Set("k1", k1)
	qs.Set("pr", bolt11)
	callback.RawQuery = qs.Encode()

	resp, err := http.Get(callback.String())
	if err != nil {
		return err
	}

	var lnurlresp lnurl.LNURLResponse
	err = json.NewDecoder(resp.Body).Decode(&lnurlresp)
	if err != nil {
		return err
	}

	if lnurlresp.Status == "ERROR" {
		return errors.New(lnurlresp.Reason)
	}

	return nil
}

func (a *Service) FinishLNURLPay(satoshis int64) error {
	a.mu.Lock()
	data, ok := a.lnurlPayTempCache[a.lnurlPaying]
	a.mu.Unlock()
	if !ok {
		return errors.New("We've lost the details for this payment, please start over.")
	}

	callback := data.callback

	qs := callback.Query()
	qs.Set("amount", strconv.FormatInt(satoshis*1000, 10))
	callback.RawQuery = qs.Encode()

	log.Print("FINISH LNURLPAY ", callback.String())

	resp, err := http.Get(callback.String())
	if err != nil {
		return err
	}

	var lnurlresp lnurl.LNURLPayResponse2
	err = json.NewDecoder(resp.Body).Decode(&lnurlresp)
	if err != nil {
		return err
	}

	log.Print("LNURLPAY WITH PR ", lnurlresp)

	if lnurlresp.Status == "ERROR" {
		return errors.New(lnurlresp.Reason)
	}

	lnclient := a.daemonAPI.APIClient()
	decodedReq, err := lnclient.DecodePayReq(context.Background(), &lnrpc.PayReqString{PayReq: lnurlresp.PR})
	if err != nil {
		return err
	}

	if decodedReq.NumSatoshis != satoshis {
		return fmt.Errorf("Invoice mismatch. Expected %d satoshis, got %d.",
			satoshis, decodedReq.NumSatoshis)
	}

	if decodedReq.DescriptionHash != data.metadataHash {
		return fmt.Errorf("Invoice mismatch. Expected description hash %s, got %s.",
			data.metadataHash, decodedReq.DescriptionHash)
	}

	// store more things for later saving on payments db
	data.success = lnurlresp.SuccessAction
	data.paymentHash = decodedReq.PaymentHash
	data.repeatable = lnurlresp.Disposable != nil && *lnurlresp.Disposable == false
	log.Print("saving lnurlpay stuff ", data)
	a.mu.Lock()
	// this time under paymentHash so we find it faster later
	a.lnurlPayTempCache[data.paymentHash] = data
	a.mu.Unlock()

	// queue cleanup of that cache entry
	go func() {
		time.Sleep(10 * time.Minute)
		a.mu.Lock()
		delete(a.lnurlPayTempCache, data.paymentHash)
		a.mu.Unlock()
	}()

	log.Print("proceeding to payment of lnurlpay invoice")
	_, err = a.SendPaymentForRequest(lnurlresp.PR, 0)

	return err
}
