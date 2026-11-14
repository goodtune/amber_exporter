// Package amber is a small client for the Amber Electric public API.
//
// The API is described at https://app.amber.com.au/swagger.json. Only the
// parts the exporter needs are modelled here.
package amber

import "time"

// Channel types reported by the API.
const (
	ChannelGeneral        = "general"
	ChannelControlledLoad = "controlledLoad"
	ChannelFeedIn         = "feedIn"
)

// Interval types reported by the API.
const (
	TypeActual   = "ActualInterval"
	TypeCurrent  = "CurrentInterval"
	TypeForecast = "ForecastInterval"
)

// Site statuses reported by the API.
const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusClosed  = "closed"
)

// Usage qualities reported by the API.
const (
	QualityBillable  = "billable"
	QualityEstimated = "estimated"
)

// Site is a property linked to the account.
type Site struct {
	ID             string    `json:"id"`
	NMI            string    `json:"nmi"`
	Channels       []Channel `json:"channels"`
	Network        string    `json:"network"`
	Status         string    `json:"status"`
	ActiveFrom     string    `json:"activeFrom"`
	ClosedOn       string    `json:"closedOn"`
	IntervalLength float64   `json:"intervalLength"`
}

// Channel is one meter channel of a site.
type Channel struct {
	Identifier string `json:"identifier"`
	Type       string `json:"type"`
	Tariff     string `json:"tariff"`
}

// TariffInformation describes how the tariff applies to an interval.
type TariffInformation struct {
	Period       string  `json:"period"`
	Season       string  `json:"season"`
	Block        float64 `json:"block"`
	DemandWindow *bool   `json:"demandWindow"`
}

// AdvancedPrice is Amber's own forecast band, in c/kWh.
type AdvancedPrice struct {
	Low       float64 `json:"low"`
	Predicted float64 `json:"predicted"`
	High      float64 `json:"high"`
}

// Interval is one priced interval on one channel.
//
// Prices are in c/kWh and include GST. The API reports them from the point
// of view of what the customer pays: a negative feed-in price means the
// customer is paid to export.
type Interval struct {
	Type              string             `json:"type"`
	Duration          int                `json:"duration"`
	SpotPerKwh        float64            `json:"spotPerKwh"`
	PerKwh            float64            `json:"perKwh"`
	Date              string             `json:"date"`
	NemTime           time.Time          `json:"nemTime"`
	StartTime         time.Time          `json:"startTime"`
	EndTime           time.Time          `json:"endTime"`
	Renewables        float64            `json:"renewables"`
	ChannelType       string             `json:"channelType"`
	TariffInformation *TariffInformation `json:"tariffInformation"`
	SpikeStatus       string             `json:"spikeStatus"`
	Descriptor        string             `json:"descriptor"`
	Estimate          *bool              `json:"estimate"`
	AdvancedPrice     *AdvancedPrice     `json:"advancedPrice"`
}

// Start returns the nominal start of the interval.
//
// The API reports startTime one second after the boundary (for example
// 03:35:01 for the interval that ends at 03:40:00), so the start is derived
// from the end time and the duration.
func (i Interval) Start() time.Time {
	return i.EndTime.Add(-time.Duration(i.Duration) * time.Minute)
}

// Usage is the metered energy and its cost for one interval on one channel.
//
// Cost is in cents and includes GST. Feed-in cost is negative when the
// customer was paid for the export.
type Usage struct {
	Interval
	ChannelIdentifier string  `json:"channelIdentifier"`
	Kwh               float64 `json:"kwh"`
	Quality           string  `json:"quality"`
	Cost              float64 `json:"cost"`
}

// PriceOptions selects what the current-prices endpoint returns.
// Zero values leave the parameter out of the request.
type PriceOptions struct {
	// Next is the number of forecast intervals to return.
	Next int
	// Previous is the number of past intervals to return.
	Previous int
	// Resolution is the interval length in minutes: 5 or 30.
	Resolution int
}

// RateLimit is the most recent rate limit state reported by the API.
type RateLimit struct {
	// Limit is the number of requests allowed in a window.
	Limit int
	// Remaining is the number of requests left in the current window.
	Remaining int
	// Reset is when the current window ends.
	Reset time.Time
	// Known is false until the API has reported a rate limit.
	Known bool
}
