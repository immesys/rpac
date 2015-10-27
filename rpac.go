package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"
)

type Config struct {
	//'static' or 'dhcp' or 'leave'
	EthMode string `yaml:"eth_mode"`
	//eg 10.4.10.170
	EthIp string `yaml:"eth_ip"`
	//eg 10.4.10.1
	EthGateway string `yaml:"eth_gateway"`

	//'static' or 'dhcp' or 'leave'
	WifiMode    string `yaml:"wifi_mode"`
	WifiIp      string `yaml:"wifi_ip"`
	WifiGateway string `yaml:"wifi_gateway"`
	//'eg' EECS-PSK
	WifiSSID string `yaml:"wifi_ssid"`
	WifiPSK  string `yaml:"wifi_psk"`

	ServiceParams map[string]interface{}
}

var blinks chan int

const BLINK_BUSY = 1
const BLINK_SUCCESS = 2
const BLINK_ERROR = 3
const BLINK_NOCONFIG = 4
const BLINK_NOINET = 5

func setupLed() {
	ioutil.WriteFile("/sys/class/leds/led0/trigger", []byte("none"), 0666)
	blinks = make(chan int, 3)
	curval := 0
	go func() {
		for {
			var ok bool
			select {
			case curval, ok = <-blinks:
				if !ok {
					os.Exit(1)
				}
				fmt.Println("got curval: ", curval)
			default:
			}
			for i := 0; i < curval; i++ {
				ioutil.WriteFile("/sys/class/leds/led0/brightness", []byte("1"), 0666)
				time.Sleep(80 * time.Millisecond)
				ioutil.WriteFile("/sys/class/leds/led0/brightness", []byte("0"), 0666)
				time.Sleep(270 * time.Millisecond)
			}
			time.Sleep(900 * time.Millisecond)
		}
	}()
}

func die(code int) {
	blinks <- code
	time.Sleep(10 * time.Second)
	close(blinks)
	for {
		runtime.Gosched()
	}
}
func goconf() {
	err := os.MkdirAll("/mnt/rpac", 0755)
	if err != nil {
		log.Println("mkdir error: ", err)
		die(BLINK_ERROR)
	}
	err = syscall.Mount("/dev/sda1", "/mnt/rpac", "vfat", 0, "")
	if err != nil {
		log.Println("mount error: ", err)
		die(BLINK_ERROR)
	}
	cfgfile, err := ioutil.ReadFile("/mnt/rpac/config.yml")
	if err != nil {
		log.Println("config file error: ", err)
		umount()
		die(BLINK_NOCONFIG)
	}
	var conf Config
	err = yaml.Unmarshal(cfgfile, &conf)
	if err != nil {
		log.Println("config file error: ", err)
		umount()
		die(BLINK_NOCONFIG)
	}
	log.Printf("config: %+v\n", conf)
	processConf(&conf)
}
func umount() {
	err := syscall.Unmount("/mnt/rpac", syscall.MNT_DETACH)
	if err != nil {
		log.Println("umount err: ", err)
	}
}
func processConf(conf *Config) {
	cr, err := os.Create("/mnt/rpac/config.log")
	if err != nil {
		log.Println("Could not create report log: ", err)
		umount()
		die(BLINK_ERROR)
	}
	oerr := func(msg string, err error) {
		log.Println(msg, err)
		cr.WriteString("ERROR: " + msg + ": " + err.Error())
		cr.Close()
		umount()
		die(BLINK_ERROR)
	}
	ncr, err := os.Create("/etc/network/interfaces.new")
	if err != nil {
		oerr("interfaces file error: ", err)
	}
	oldconf, err := os.Open("/etc/network/interfaces")
	if err != nil {
		oerr("interfaces file error: ", err)
	}
	ocr := bufio.NewReader(oldconf)
	mode := "preamble"
	oline := func(v string) {
		ncr.WriteString(v + "\n")
		cr.WriteString("/etc/network/interfaces: " + v + "\n")
	}
	observe := func(tag string, cmd ...string) {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			cr.WriteString(tag + " exec fail: " + err.Error())
		}
		cr.WriteString(tag + " output:\n")
		cr.Write(out)
		cr.WriteString("\n")
	}
cfgloop:
	for rline, _, err := ocr.ReadLine(); err == nil; rline, _, err = ocr.ReadLine() {
		line := string(rline)
	rematch:
		switch mode {
		case "preamble":
			oline(line)
			if strings.HasPrefix(line, "#RPAC ETH") {
				mode = "eth"
				goto rematch
			}
		case "eth":
			if conf.EthMode != "leave" && conf.EthMode != "down" {
				//write eth config
				oline("auto eth0")
				if conf.EthMode == "static" {
					oline("iface eth0 inet static")
					oline("  address " + conf.EthIp)
					oline("  gateway " + conf.EthGateway)
					oline("  netmask 255.255.255.0")
					oline("  dns-nameservers 8.8.8.8 8.8.4.4")
				} else {
					oline("iface eth0 inet dhcp")
				}
			}
			mode = "posteth"
		case "posteth":
			if strings.HasPrefix(line, "#RPAC WIFI") {
				oline(line)
				mode = "wifi"
				goto rematch
			} else if conf.EthMode == "leave" {
				oline(line)
			}
		case "wifi":
			if conf.WifiMode != "leave" && conf.WifiMode != "down" {
				oline("auto wlan0")
				if conf.WifiMode == "static" {
					oline("iface wlan0 inet static")
					oline("  address " + conf.WifiIp)
					oline("  gateway " + conf.WifiGateway)
					oline("  netmask 255.255.255.0")
					oline("  dns-nameservers 8.8.8.8 8.8.4.4")
				} else {
					oline("iface wlan0 inet dhcp")
				}
				oline("  wpa-ssid " + conf.WifiSSID)
				oline("  wpa-psk " + conf.WifiPSK)
				break cfgloop
			} else if conf.WifiMode == "down" {
				break cfgloop
			} else {
				mode = "postwifi"
			}
		case "postwifi":
			oline(line)
		}
	}
	ncr.Close()
	oldconf.Close()

	err = os.Rename("/etc/network/interfaces.new", "/etc/network/interfaces")
	if err != nil {
		oerr("Could not switchover interfaces", err)
	}

	blinks <- BLINK_BUSY

	if conf.WifiMode != "leave" || conf.WifiMode == "down" {
		observe("ifdown wlan0", "/sbin/ifdown", "wlan0")
		observe("linkdown wlan0", "/sbin/ip", "link", "set", "wlan0", "down")
	}
	if conf.EthMode != "leave" || conf.EthMode == "down" {
		observe("ifdown eth0", "/sbin/ifdown", "eth0")
		observe("linkdown eth0", "/sbin/ip", "link", "set", "eth0", "down")
	}

	if conf.WifiMode == "dhcp" || conf.WifiMode == "static" {
		observe("ifup wlan0", "/sbin/ifup", "wlan0")
	}

	if conf.EthMode == "dhcp" || conf.EthMode == "static" {
		observe("ifup eth0", "/sbin/ifup", "eth0")
	}

	time.Sleep(5 * time.Second)
	observe("ifconfig", "/sbin/ifconfig", "-a")
	observe("iplink", "/sbin/ip", "link", "show")
	observe("iproute", "/sbin/ip", "route")
	out, err := exec.Command("/bin/ping", "-w5", "-i0.5", "google.com").CombinedOutput()
	if err != nil {
		cr.WriteString("ping exec fail: " + err.Error())
		blinks <- BLINK_NOINET
	} else {
		blinks <- BLINK_SUCCESS
	}
	cr.WriteString("ping:\n")
	cr.Write(out)
	cr.WriteString("\n")

	observe("cpfiles", "/bin/bash", "-c", "cp -rT /mnt/rpac/files/ /")
	observe("svcreload", "/usr/sbin/service", "supervisor", "restart")
	observe("svcstatus", "/usr/bin/supervisorctl", "status")
	cr.Close()
	umount()
	time.Sleep(10 * time.Second)
	close(blinks)
	time.Sleep(10 * time.Second)
}
func hacks() {
	//Try improve network resiliency by disabling IPv6
	ioutil.WriteFile("/proc/sys/net/ipv6/conf/all/disable_ipv6", []byte("1"), 0666)
	ioutil.WriteFile("/proc/sys/net/ipv6/conf/default/disable_ipv6", []byte("1"), 0666)
}
func main() {
	logwriter, e := syslog.New(syslog.LOG_CRIT, "rpac")
	if e == nil {
		log.SetOutput(logwriter)
	}
	hacks()
	setupLed()
	goconf()
	time.Sleep(2 * time.Second)
}
