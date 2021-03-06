package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	binance "github.com/adshao/go-binance"
	"github.com/alpacahq/marketstore/executor"
	"github.com/alpacahq/marketstore/planner"
	"github.com/alpacahq/marketstore/plugins/bgworker"
	"github.com/alpacahq/marketstore/utils"
	"github.com/alpacahq/marketstore/utils/io"
	"github.com/golang/glog"
)

var suffixBinanceDefs = map[string]string{
	"Min": "m",
	"H":   "h",
	"D":   "d",
	"W":   "w",
}

// ExchangeInfo exchange info
type ExchangeInfo struct {
	Timezone   string `json:"timezone"`
	ServerTime int64  `json:"serverTime"`
	RateLimits []struct {
		RateLimitType string `json:"rateLimitType"`
		Interval      string `json:"interval"`
		Limit         int    `json:"limit"`
	} `json:"rateLimits"`
	ExchangeFilters []interface{} `json:"exchangeFilters"`
	Symbols         []struct {
		Symbol             string   `json:"symbol"`
		Status             string   `json:"status"`
		BaseAsset          string   `json:"baseAsset"`
		BaseAssetPrecision int      `json:"baseAssetPrecision"`
		QuoteAsset         string   `json:"quoteAsset"`
		QuotePrecision     int      `json:"quotePrecision"`
		OrderTypes         []string `json:"orderTypes"`
		IcebergAllowed     bool     `json:"icebergAllowed"`
		Filters            []struct {
			FilterType       string `json:"filterType"`
			MinPrice         string `json:"minPrice,omitempty"`
			MaxPrice         string `json:"maxPrice,omitempty"`
			TickSize         string `json:"tickSize,omitempty"`
			MinQty           string `json:"minQty,omitempty"`
			MaxQty           string `json:"maxQty,omitempty"`
			StepSize         string `json:"stepSize,omitempty"`
			MinNotional      string `json:"minNotional,omitempty"`
			Limit            int    `json:"limit,omitempty"`
			MaxNumAlgoOrders int    `json:"maxNumAlgoOrders,omitempty"`
		} `json:"filters"`
	} `json:"symbols"`
}

// Get JSON via http request and decodes it using NewDecoder. Sets target interface to decoded json
func getJson(url string, target interface{}) error {
	var myClient = &http.Client{Timeout: 10 * time.Second}
	r, err := myClient.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	return json.NewDecoder(r.Body).Decode(target)
}

// For ConvertStringToFloat function and Run() function to making exiting easier
var errorsConversion []error

// FetcherConfig is a structure of binancefeeder's parameters
type FetcherConfig struct {
	Symbols       []string `json:"symbols"`
	BaseCurrency  string   `json:"base_currency"`
	QueryStart    string   `json:"query_start"`
	BaseTimeframe string   `json:"base_timeframe"`
}

// BinanceFetcher is the main worker for Binance
type BinanceFetcher struct {
	config        map[string]interface{}
	symbols       []string
	baseCurrency  string
	queryStart    time.Time
	baseTimeframe *utils.Timeframe
}

// recast changes parsed JSON-encoded data represented as an interface to FetcherConfig structure
func recast(config map[string]interface{}) *FetcherConfig {
	data, _ := json.Marshal(config)
	ret := FetcherConfig{}
	json.Unmarshal(data, &ret)
	return &ret
}

//Convert string to float64 using strconv
func convertStringToFloat(str string) float64 {
	convertedString, err := strconv.ParseFloat(str, 64)
	//Store error in string array which will be checked in main fucntion later to see if there is a need to exit
	if err != nil {
		glog.Errorf("String to float error: %v", err)
		errorsConversion = append(errorsConversion, err)
	}
	return convertedString
}

//Checks time string and returns correct time format
func queryTime(query string) time.Time {
	trials := []string{
		"2006-01-02 03:04:05",
		"2006-01-02T03:04:05",
		"2006-01-02 03:04",
		"2006-01-02T03:04",
		"2006-01-02",
	}
	for _, layout := range trials {
		qs, err := time.Parse(layout, query)
		if err == nil {
			//Returns time in correct time.Time object once it matches correct time format
			return qs.In(utils.InstanceConfig.Timezone)
		}
	}
	//Return null if no time matches time format
	return time.Time{}
}

//Convert time from milliseconds to Unix
func convertMillToTime(originalTime int64) time.Time {
	i := time.Unix(0, originalTime*int64(time.Millisecond))
	return i
}

// Append if String is Missing from array
// All credit to Sonia: https://stackoverflow.com/questions/9251234/go-append-if-unique
func appendIfMissing(slice []string, i string) ([]string, bool) {
	for _, ele := range slice {
		if ele == i {
			return slice, false
		}
	}
	return append(slice, i), true
}

//Gets all symbols from binance
func getAllSymbols(quoteAsset string) []string {
	client := binance.NewClient("", "")
	m := ExchangeInfo{}
	err := getJson("https://api.binance.com/api/v1/exchangeInfo", &m)
	symbol := make([]string, 0)
	status := make([]string, 0)
	validSymbols := make([]string, 0)
	tradingSymbols := make([]string, 0)
	quote := ""

	if err != nil {
		glog.Errorf("Binance /exchangeInfo API error: %v", err)
		tradingSymbols = []string{"EOS", "TRX", "ONT", "XRP", "ADA",
			"LTC", "BCC", "TUSD", "IOTA", "ETC", "ICX", "NEO", "XLM", "QTUM"}
	} else {
		for _, info := range m.Symbols {
			quote = info.QuoteAsset
			notRepeated := true
			// Check if data is the right base currency and then check if it's already recorded
			if quote == quoteAsset {
				symbol, notRepeated = appendIfMissing(symbol, info.BaseAsset)
				if notRepeated {
					status = append(status, info.Status)
				}
			}
		}

		//Check status and append to symbols list if valid
		for index, s := range status {
			if s == "TRADING" {
				tradingSymbols = append(tradingSymbols, symbol[index])
			}
		}
	}

	// Double check each symbol is working as intended
	for _, s := range tradingSymbols {
		_, err := client.NewKlinesService().Symbol(s + quoteAsset).Interval("1m").Do(context.Background())
		if err == nil {
			validSymbols = append(validSymbols, s)
		}
	}

	return validSymbols
}

func findLastTimestamp(symbol string, tbk *io.TimeBucketKey) time.Time {
	cDir := executor.ThisInstance.CatalogDir
	query := planner.NewQuery(cDir)
	query.AddTargetKey(tbk)
	start := time.Unix(0, 0).In(utils.InstanceConfig.Timezone)
	end := time.Unix(math.MaxInt64, 0).In(utils.InstanceConfig.Timezone)
	query.SetRange(start.Unix(), end.Unix())
	query.SetRowLimit(io.LAST, 1)
	parsed, err := query.Parse()
	if err != nil {
		return time.Time{}
	}
	reader, err := executor.NewReader(parsed)
	csm, _, err := reader.Read()
	cs := csm[*tbk]
	if cs == nil || cs.Len() == 0 {
		return time.Time{}
	}
	ts := cs.GetTime()
	return ts[0]
}

// NewBgWorker registers a new background worker
func NewBgWorker(conf map[string]interface{}) (bgworker.BgWorker, error) {
	config := recast(conf)
	var queryStart time.Time
	timeframeStr := "1Min"
	var symbols []string
	baseCurrency := "BNB"

	if config.BaseTimeframe != "" {
		timeframeStr = config.BaseTimeframe
	}

  // // Creslin - hard coding plugins to a quote currency ... names base in here. :/
	// if config.BaseCurrency != "" {
	// 	baseCurrency = config.BaseCurrency
	// }

	if config.QueryStart != "" {
		queryStart = queryTime(config.QueryStart)
	}

	//First see if config has symbols, if not retrieve all from binance as default
	if len(config.Symbols) > 0 {
		symbols = config.Symbols
	} else {
		symbols = getAllSymbols(baseCurrency)
	}

	return &BinanceFetcher{
		config:        conf,
		baseCurrency:  baseCurrency,
		symbols:       symbols,
		queryStart:    queryStart,
		baseTimeframe: utils.NewTimeframe(timeframeStr),
	}, nil
}

// Run grabs data in intervals from starting time to ending time.
// If query_end is not set, it will run forever.
func (bn *BinanceFetcher) Run() {
	symbols := bn.symbols
	client := binance.NewClient("", "")
	timeStart := time.Time{}
	baseCurrency := bn.baseCurrency
	slowDown := false

	// Get correct Time Interval for Binance
	originalInterval := bn.baseTimeframe.String
	re := regexp.MustCompile("[0-9]+")
	re2 := regexp.MustCompile("[a-zA-Z]+")
	timeIntervalLettersOnly := re.ReplaceAllString(originalInterval, "")
	timeIntervalNumsOnly := re2.ReplaceAllString(originalInterval, "")
	correctIntervalSymbol := suffixBinanceDefs[timeIntervalLettersOnly]
	if len(correctIntervalSymbol) <= 0 {
		glog.Errorf("Interval Symbol Format Incorrect. Setting to time interval to default '1Min'")
		correctIntervalSymbol = "1Min"
	}
	timeInterval := timeIntervalNumsOnly + correctIntervalSymbol

	// Get last timestamp collected
	for _, symbol := range symbols {
		tbk := io.NewTimeBucketKey("BINANCE_BNB_" + symbol + "/" + bn.baseTimeframe.String + "/OHLCV")
		lastTimestamp := findLastTimestamp(symbol, tbk)
		glog.Infof("lastTimestamp for %s = %v", symbol, lastTimestamp)
		if timeStart.IsZero() || (!lastTimestamp.IsZero() && lastTimestamp.Before(timeStart)) {
			timeStart = lastTimestamp
		}
	}

	// Set start time if not given.
	if !bn.queryStart.IsZero() {
		timeStart = bn.queryStart
	} else {
		timeStart = time.Now().UTC().Add(-bn.baseTimeframe.Duration)
	}

	// For loop for collecting candlestick data forever
	// Note that the max amount is 1000 candlesticks which is no problem
	var timeStartM int64
	var timeEndM int64
	var timeEnd time.Time
	var originalTimeStart time.Time
	var originalTimeEnd time.Time
	var originalTimeEndZero time.Time
	var waitTill time.Time
	firstLoop := true

	for {
		// finalTime = time.Now().UTC()
		originalTimeStart = timeStart
		originalTimeEnd = timeEnd

		// Check if it's finished backfilling. If not, just do 300 * Timeframe.duration
		// only do beyond 1st loop
		if !slowDown {
			if !firstLoop {
				timeStart = timeStart.Add(bn.baseTimeframe.Duration * 300)
				timeEnd = timeStart.Add(bn.baseTimeframe.Duration * 300)
			} else {
				firstLoop = false
				// Keep timeStart as original value
				timeEnd = timeStart.Add(bn.baseTimeframe.Duration * 300)
			}
			if timeEnd.After(time.Now().UTC()) {
				slowDown = true
			}
		} else {
			// Set to the :00 of previous TimeEnd to ensure that the complete candle that was not formed before is written
			originalTimeEnd = originalTimeEndZero
		}

		// Sleep for the timeframe
		// Otherwise continue to call every second to backfill the data
		// Slow Down for 1 Duration period
		// Make sure last candle is formed
		if slowDown {
			timeEnd = time.Now().UTC()
			timeStart = originalTimeEnd

			year := timeEnd.Year()
			month := timeEnd.Month()
			day := timeEnd.Day()
			hour := timeEnd.Hour()
			minute := timeEnd.Minute()

			// To prevent gaps (ex: querying between 1:31 PM and 2:32 PM (hourly)would not be ideal)
			// But we still want to wait 1 candle afterwards (ex: 1:01 PM (hourly))
			// If it is like 1:59 PM, the first wait sleep time will be 1:59, but afterwards would be 1 hour.
			// Main goal is to ensure it runs every 1 <time duration> at :00
			switch originalInterval {
			case "1Min":
				timeEnd = time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
			case "1H":
				timeEnd = time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
			case "1D":
				timeEnd = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
			default:
				glog.Infof("Incorrect format: %v", originalInterval)
			}
			waitTill = timeEnd.Add(bn.baseTimeframe.Duration)

			timeStartM := timeStart.UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
			timeEndM := timeEnd.UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))

			// Make sure you get the last candle within the timeframe.
			// If the next candle is in the API call, that means the previous candle has been fully formed
			// (ex: if we see :00 is formed that means the :59 candle is fully formed)
			gotCandle := false
			for !gotCandle {
				rates, err := client.NewKlinesService().Symbol(symbols[0] + baseCurrency).Interval(timeInterval).StartTime(timeStartM).Do(context.Background())
				if err != nil {
					glog.Errorf("Response error: %v", err)
					time.Sleep(time.Minute)
				}

				if len(rates) > 0 && rates[len(rates)-1].OpenTime-timeEndM >= 0 {
					gotCandle = true
				}
			}

			originalTimeEndZero = timeEnd
			// Change timeEnd to the correct time where the last candle is formed
			timeEnd = time.Now().UTC()
		}

		// Repeat since slowDown loop won't run if it hasn't been past the current time
		timeStartM = timeStart.UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
		timeEndM = timeEnd.UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))

		for _, symbol := range symbols {
			// glog.Infof("Requesting %s %v - %v", symbol, timeStart, timeEnd)
			rates, err := client.NewKlinesService().Symbol(symbol + baseCurrency).Interval(timeInterval).StartTime(timeStartM).EndTime(timeEndM).Do(context.Background())
			if err != nil {
				glog.Errorf("Response error: %v", err)
				glog.Infof("Problematic symbol %s", symbol)
				time.Sleep(time.Minute)
				// Go back to last time
				timeStart = originalTimeStart
				continue
			}
			// if len(rates) == 0 {
			// 	glog.Info("len(rates) == 0")
			// 	continue
			// }
			openTime := make([]int64, 0)
			open := make([]float64, 0)
			high := make([]float64, 0)
			low := make([]float64, 0)
			close := make([]float64, 0)
			volume := make([]float64, 0)
			// closeTime := make([]int64, 0)
			// quoteAssetVolume := make([]float64, 0)
			// tradeNum := make([]int64, 0)
			// takerBuyBaseAssetVolume := make([]float64, 0)
			// takerBuyQuoteAssetVolume := make([]float64, 0)
			for _, rate := range rates {
				errorsConversion = errorsConversion[:0]
				// if nil, do not append to list
				if rate.OpenTime != 0 && rate.Open != "" &&
					rate.High != "" && rate.Low != "" &&
					rate.Close != "" && rate.Volume != "" {
					openTime = append(openTime, convertMillToTime(rate.OpenTime).Unix())
					open = append(open, convertStringToFloat(rate.Open))
					high = append(high, convertStringToFloat(rate.High))
					low = append(low, convertStringToFloat(rate.Low))
					close = append(close, convertStringToFloat(rate.Close))
					volume = append(volume, convertStringToFloat(rate.Volume))
  				// closeTime = append(closeTime, convertMillToTime(rate.CloseTime).Unix())
  				// quoteAssetVolume = append(quoteAssetVolume, convertStringToFloat(rate.QuoteAssetVolume))
  				// tradeNum = append(tradeNum, rate.TradeNum)
  				// takerBuyBaseAssetVolume  = append(takerBuyBaseAssetVolume, convertStringToFloat(rate.TakerBuyBaseAssetVolume))
  				// takerBuyQuoteAssetVolume  = append(takerBuyQuoteAssetVolume, convertStringToFloat(rate.TakerBuyQuoteAssetVolume))
					for _, e := range errorsConversion {
						if e != nil {
							return
						}
					}
				} else {
					glog.Infof("No value in rate %v", rate)
				}
			}

			validWriting := true
			if len(openTime) == 0 || len(open) == 0 || len(high) == 0 || len(low) == 0 || len(close) == 0 || len(volume) == 0 {
				validWriting = false
			}
			// if data is nil, do not write to csm
			if validWriting {
				cs := io.NewColumnSeries()
				// Remove last incomplete candle if it exists since that is incomplete
				// Since all are the same length we can just check one
				// We know that the last one on the list is the incomplete candle because in
				// the gotCandle loop we only move on when the incomplete candle appears which is the last entry from the API
				if slowDown && len(openTime) > 1 {
					openTime = openTime[:len(openTime)-1]
					open = open[:len(open)-1]
					high = high[:len(high)-1]
					low = low[:len(low)-1]
					close = close[:len(close)-1]
					volume = volume[:len(volume)-1]
  				// closeTime = closeTime[:len(closeTime)-1]
  				// quoteAssetVolume = quoteAssetVolume[:len(QuoteAssetVolume)-1]
  				// tradeNum = tradeNum[:len(TradeNum)-1]
  				// takerBuyBaseAssetVolume = takerBuyBaseAssetVolume[:len(TakerBuyBaseAssetVolume)-1]
  				// takerBuyQuoteAssetVolume = takerBuyQuoteAssetVolume[:len(TakerBuyQuoteAssetVolume)-1]
				}
				cs.AddColumn("Epoch", openTime)
				cs.AddColumn("Open", open)
				cs.AddColumn("High", high)
				cs.AddColumn("Low", low)
				cs.AddColumn("Close", close)
				cs.AddColumn("Volume", volume)
        // cs.AddColumn("closeTime", closeTime)
  			// cs.AddColumn("quoteAssetVolume", quoteAssetVolume)
  			// cs.AddColumn("tradeNum", tradeNum)
  			// cs.AddColumn("takerBuyBaseAssetVolume", takerBuyBaseAssetVolume)
  			// cs.AddColumn("takerBuyQuoteAssetVolume", takerBuyQuoteAssetVolume)
				csm := io.NewColumnSeriesMap()
  			// creslin change from symbol to exchange_symbol_quote
				tbk := io.NewTimeBucketKey("BINANCE_BNB_" + symbol + "/" + bn.baseTimeframe.String + "/OHLCV")
				csm.AddColumnSeries(*tbk, cs)
				executor.WriteCSM(csm, false)
			}

		}

		if slowDown {
			// Sleep till next :00 time
			time.Sleep(waitTill.Sub(time.Now().UTC()))
		} else {
			// Binance rate limit is 20 reequests per second so this shouldn't be an issue.
      			// Changed to 100msec - Creslin
			time.Sleep(time.Second * 10)
		}

	}
}

func main() {
	// symbol := "BNB"
	// interval := "1m"
	// baseCurrency := "BNB"
	//
	// client := binance.NewClient("", "")
	// klines, err := client.NewKlinesService().Symbol(symbol + baseCurrency).
	// 	Interval(interval).Do(context.Background())
	// if err != nil {
	// 	fmt.Println(err)
	// 	return
	// }
	// for _, k := range klines {
	// 	fmt.Println(k)
	// }
	// symbols := getAllSymbols("BNB")
	// for _, s := range symbols {
	// 	fmt.Println(s)
	// }
}

