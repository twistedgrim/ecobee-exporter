// Package collector provides Prometheus support for Ecobee metrics.
// see for Ecobee API: https://www.ecobee.com/home/developer/api/introduction/index.shtml
package collector

import (
	"fmt"
	"reflect"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twistedgrim/go-ecobee/ecobee"
)

type descs string

func (d descs) new(fqName, help string, variableLabels []string) *prometheus.Desc {
	return prometheus.NewDesc(fmt.Sprintf("%s_%s", d, fqName), help, variableLabels, nil)
}

// eCollector implements prometheus.eCollector to gather ecobee metrics on-demand.
type eCollector struct {
	client *ecobee.Client

	// per-query descriptors
	fetchTime *prometheus.Desc

	// runtime descriptors
	actualTemperature, targetTemperatureMin, targetTemperatureMax, currentHvacMode, holdTempMetric, hvacInOperation *prometheus.Desc

	// sensor descriptors
	temperature, humidity, occupancy, inUse *prometheus.Desc
}

// NewEcobeeCollector returns a new eCollector with the given prefix assigned to all
// metrics. Note that Prometheus metrics must be unique! Don't try to create
// two Collectors with the same metric prefix.
func NewEcobeeCollector(c *ecobee.Client, metricPrefix string) *eCollector {
	d := descs(metricPrefix)

	// fields common across multiple metrics
	runtime := []string{"thermostat_id", "thermostat_name"}
	sensor := append(runtime, "sensor_id", "sensor_name", "sensor_type")

	return &eCollector{
		client: c,

		// collector metrics
		fetchTime: d.new(
			"fetch_time",
			"Elapsed time fetching data via Ecobee API",
			nil,
		),

		// thermostat (aka runtime) metrics
		actualTemperature: d.new(
			"actual_temperature",
			"Thermostat-averaged current temperature",
			runtime,
		),
		targetTemperatureMax: d.new(
			"target_temperature_max",
			"Maximum temperature for thermostat to maintain",
			runtime,
		),
		targetTemperatureMin: d.new(
			"target_temperature_min",
			"Minimum temperature for thermostat to maintain",
			runtime,
		),
		currentHvacMode: d.new(
			"current_hvac_mode",
			"Current HVAC mode of thermostat",
			[]string{"thermostat_id", "thermostat_name", "current_hvac_mode"},
		),
		holdTempMetric: d.new(
			"hold_temperature",
			"Temperature to hold by thermostat",
			[]string{"thermostat_id", "thermostat_name", "type"},
		),
		hvacInOperation: d.new(
			"hvac_in_operation",
			"HVAC equipment running status (0 or 1)",
			[]string{"thermostat_id", "thermostat_name", "equipment"},
		),

		// sensor metrics
		temperature: d.new(
			"temperature",
			"Temperature reported by a sensor in degrees",
			sensor,
		),
		humidity: d.new(
			"humidity",
			"Humidity reported by a sensor in percent",
			sensor,
		),
		occupancy: d.new(
			"occupancy",
			"Occupancy reported by a sensor (0 or 1)",
			sensor,
		),
		inUse: d.new(
			"in_use",
			"Is sensor being used in thermostat calculations (0 or 1)",
			sensor,
		),
	}
}

// Describe dumps all metric descriptors into ch.
func (c *eCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.fetchTime
	ch <- c.actualTemperature
	ch <- c.targetTemperatureMax
	ch <- c.targetTemperatureMin
	ch <- c.temperature
	ch <- c.humidity
	ch <- c.occupancy
	ch <- c.inUse
	ch <- c.currentHvacMode
	ch <- c.holdTempMetric
	ch <- c.hvacInOperation
}

// Collect retrieves thermostat data via the ecobee API.
func (c *eCollector) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	tt, err := c.client.GetThermostats(ecobee.Selection{
		SelectionType:   "registered",
		IncludeSensors:  true,
		IncludeRuntime:  true,
		IncludeSettings: true,
		IncludeEvents:   true,
	})
	elapsed := time.Now().Sub(start)
	ch <- prometheus.MustNewConstMetric(c.fetchTime, prometheus.GaugeValue, elapsed.Seconds())
	if err != nil {
		log.Error(err)
		return
	}
	for _, t := range tt {
		tFields := []string{t.Identifier, t.Name}
		if t.Runtime.Connected {
			ch <- prometheus.MustNewConstMetric(
				c.actualTemperature, prometheus.GaugeValue, float64(t.Runtime.ActualTemperature)/10, tFields...,
			)
			ch <- prometheus.MustNewConstMetric(
				c.targetTemperatureMax, prometheus.GaugeValue, float64(t.Runtime.DesiredCool)/10, tFields...,
			)
			ch <- prometheus.MustNewConstMetric(
				c.targetTemperatureMin, prometheus.GaugeValue, float64(t.Runtime.DesiredHeat)/10, tFields...,
			)
			ch <- prometheus.MustNewConstMetric(
				c.currentHvacMode, prometheus.GaugeValue, 0, t.Identifier, t.Name, t.Settings.HvacMode,
			)
			if t.Settings.HvacMode != "off" {
				for _, event := range t.Events {
					if event.Running && event.Type == "hold" {
						if !event.IsCoolOff && t.Settings.HvacMode != "heat" {
							ch <- prometheus.MustNewConstMetric(
								c.holdTempMetric, prometheus.GaugeValue, float64(event.CoolHoldTemp)/10, t.Identifier, t.Name, "cool",
							)
						}
						if !event.IsHeatOff && t.Settings.HvacMode != "cool" {
							ch <- prometheus.MustNewConstMetric(
								c.holdTempMetric, prometheus.GaugeValue, float64(event.HeatHoldTemp)/10, t.Identifier, t.Name, "heat",
							)
						}
					}
				}
			}
		}
		for _, s := range t.RemoteSensors {
			sFields := append(tFields, s.ID, s.Name, s.Type)
			inUse := float64(0)
			if s.InUse {
				inUse = 1
			}
			ch <- prometheus.MustNewConstMetric(
				c.inUse, prometheus.GaugeValue, inUse, sFields...,
			)
			for _, sc := range s.Capability {
				switch sc.Type {
				case "temperature":
					if v, err := strconv.ParseFloat(sc.Value, 64); err == nil {
						ch <- prometheus.MustNewConstMetric(
							c.temperature, prometheus.GaugeValue, v/10, sFields...,
						)
					} else {
						log.Error(err)
					}
				case "humidity":
					if v, err := strconv.ParseFloat(sc.Value, 64); err == nil {
						ch <- prometheus.MustNewConstMetric(
							c.humidity, prometheus.GaugeValue, v, sFields...,
						)
					} else {
						log.Error(err)
					}
				case "occupancy":
					switch sc.Value {
					case "true":
						ch <- prometheus.MustNewConstMetric(
							c.occupancy, prometheus.GaugeValue, 1, sFields...,
						)
					case "false":
						ch <- prometheus.MustNewConstMetric(
							c.occupancy, prometheus.GaugeValue, 0, sFields...,
						)
					default:
						log.Errorf("unknown sensor occupancy value %q", sc.Value)
					}
				default:
					log.Infof("ignoring sensor capability %q", sc.Type)
				}
			}
		}
	}
	statSummary, err := c.client.GetThermostatSummary(ecobee.Selection{
		SelectionType:          "registered",
		IncludeEquipmentStatus: true,
		IncludeAlerts:          true,
	})
	if err != nil {
		log.Error(err)
		return
	}
	// sAttr := []string{"HeatPump", "HeatPump2", "HeatPump3", "CompCool1", "CompCool2", "AuxHeat1", "AuxHeat2", "AuxHeat3", "Fan", "Humidifier", "Dehumidifier", "Ventilator", "Economizer", "CompHotWater", "AuxHotWater"}
	sAttr := []string{"CompCool1", "AuxHeat1", "Fan"}
	for _, s := range statSummary {
		if s.Connected {
			r := reflect.ValueOf(s)
			for _, a := range sAttr {
				f := reflect.Indirect(r).FieldByName(a)
				switch f.Bool() {
				case true:
					ch <- prometheus.MustNewConstMetric(
						c.hvacInOperation, prometheus.GaugeValue, 1, s.Identifier, s.Name, a,
					)
				case false:
					ch <- prometheus.MustNewConstMetric(
						c.hvacInOperation, prometheus.GaugeValue, 0, s.Identifier, s.Name, a,
					)
				}
			}
		}
	}
}
