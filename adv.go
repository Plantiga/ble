package ble

// AdvHandler handles advertisement.
type AdvHandler func(a Advertisement)

// TimeoutHandler handles HCI timeouts.
type TimeoutHandler func(e error)

// AdvFilter returns true if the advertisement matches specified condition.
type AdvFilter func(a Advertisement) bool

// Advertisement ...
type Advertisement interface {
	LocalName() string
	ManufacturerData() []byte
	ServiceData() []ServiceData
	Services() []UUID
	OverflowService() []UUID
	TxPowerLevel() int
	Connectable() bool
	SolicitedService() []UUID
	RSSI() int
	Addr() Addr

	ToMap() (map[string]interface{}, error)
}

var AdvertisementMapKeys = struct {
	MAC         string
	RSSI        string
	Name        string
	MFG         string
	Services    string
	ServiceData string
	Connectable string
	Solicited   string
	EventType   string
	Flags       string
	TxPower     string
}{
	MAC:         "mac",
	RSSI:        "rssi",
	Name:        "name",
	MFG:         "mfg",
	Services:    "services",
	ServiceData: "serviceData",
	Connectable: "connectable",
	Solicited:   "solicited",
	EventType:   "eventType",
	Flags:       "flags",
	TxPower:     "txPower",
}

// ServiceData ...
type ServiceData struct {
	UUID UUID
	Data []byte
}
