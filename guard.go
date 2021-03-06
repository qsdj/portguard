/*
	date: 2015-04-21
	author: xjdrew
*/
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	tcpPacketTypeNull    string = "TCP NULL scan"
	tcpPacketTypeXMAS    string = "TCP XMAS scan"
	tcpPacketTypeSYN     string = "TCP SYN/Normal scan"
	tcpPacketTypeUnknown string = "Unknown Type: TCP Packet Flags(FIN,SYN,RST,PSH,ACK,URG): %d"
)

var (
	mode              *string
	debug             *bool
	portCacheDuration *int64 // see smartVerify for explanation
	serverIp          = net.ParseIP("0.0.0.0").To4()
	sockAddr          syscall.SockaddrInet4
	alarmLogger       *log.Logger
	blockedLogger     *log.Logger
	mainLogger        *log.Logger
	checkedPortCache  map[int]int64
	stateEngine       map[string][]int
)

var (
	cfgMinPort        int = 0
	cfgMaxPort        int = 65535
	cfgNoisyPorts     map[int]bool
	cfgExcludePorts   map[int]bool
	cfgIgnoreIps      []*net.IPNet
	cfgKillRoute      string = ""
	cfgKillRunCmd     string = ""
	cfgKillNotifyUrl  string = ""
	cfgScanTrigger    int    = 0
	cfgAlarmLogPath   string
	cfgAlarmLog       io.Writer
	cfgBlockedLog     io.Writer
	cfgBlockedLogPath string
)

func init() {
	copy(sockAddr.Addr[:], serverIp[:])
	cfgNoisyPorts = make(map[int]bool)
	cfgExcludePorts = make(map[int]bool)

	checkedPortCache = make(map[int]int64)
	stateEngine = make(map[string][]int)
}

func createLogger(extra io.Writer) *log.Logger {
	var writers []io.Writer
	if extra != nil {
		writers = append(writers, extra)
	}

	if *debug {
		writers = append(writers, io.Writer(os.Stderr))
	}

	if len(writers) > 0 {
		return log.New(io.MultiWriter(writers...), "", log.Ldate|log.Lmicroseconds)
	} else {
		return nil
	}

}

func logAlarm(format string, a ...interface{}) {
	if alarmLogger == nil {
		return
	}
	alarmLogger.Printf(format, a...)
}

func logBlocked(format string, a ...interface{}) {
	if blockedLogger == nil {
		return
	}
	blockedLogger.Printf(format, a...)
}

func logMain(exit bool, format string, a ...interface{}) {
	if mainLogger != nil {
		mainLogger.Printf(format, a...)
	} else {
		log.Printf(format, a...)
	}
	if exit {
		os.Exit(1)
	}
}

// if port is in used
// net.Listen will auto set SO_REUSEADDR when listen a port
func smartVerifyPort(port int) bool {
	stype := syscall.SOCK_STREAM
	if *mode == "udp" {
		stype = syscall.SOCK_DGRAM
	}
	fd, err := syscall.Socket(syscall.AF_INET, stype, 0)
	if err != nil {
		return false
	}
	sockAddr.Port = port
	err = syscall.Bind(fd, &sockAddr)
	syscall.Close(fd)
	if err != nil {
		return true
	}
	return false
}

// use socket and bind api to check port is very expensive
// if port is in use, we assume it'll be used as long as *portCacheDuration* seconds
// so we cache the result
func smartVerify(port int) bool {
	if *portCacheDuration <= 0 {
		return smartVerifyPort(port)
	}

	timestamp := time.Now().Unix()
	if expire, ok := checkedPortCache[port]; ok {
		if expire > timestamp {
			return true
		} else {
			delete(checkedPortCache, port)
		}
	}

	ok := smartVerifyPort(port)
	if ok {
		checkedPortCache[port] = timestamp + *portCacheDuration
	}
	return ok
}

func isExlcudePort(port int) bool {
	// check port range
	if port < cfgMinPort || port > cfgMaxPort {
		return true
	}

	_, ok := cfgExcludePorts[port]
	return ok
}

func isIgnoredIP(ip net.IP) bool {
	if cfgIgnoreIps == nil {
		return false
	}
	for _, n := range cfgIgnoreIps {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// cfgScanTrigger + 2 times scan
func isBlockedIP(ip string) bool {
	ports, ok := stateEngine[ip]
	if !ok {
		return false
	}

	if len(ports) > cfgScanTrigger {
		return true
	}
	return false
}

// true if trigger blocked
func checkStateEngine(ip string, port int) bool {
	ports, ok := stateEngine[ip]
	sz := cfgScanTrigger + 1
	if !ok {
		ports = make([]int, sz)[:0]
	}
	if len(ports) >= sz {
		return true
	}

	for _, v := range ports {
		if v == port {
			return false
		}
	}

	ports = append(ports, port)
	stateEngine[ip] = ports
	if len(ports) >= sz {
		return true
	}
	return false
}

func reportPacketType(flags uint8) *string {
	if flags == 0 {
		return &tcpPacketTypeNull
	} else if flags&(FIN|URG|PSH) == (FIN | URG | PSH) {
		return &tcpPacketTypeXMAS
	} else if flags == SYN {
		return &tcpPacketTypeSYN
	} else {
		packetType := fmt.Sprintf(tcpPacketTypeUnknown, flags)
		return &packetType
	}
}

func runExternalCommand(ip string, port int) {
	if cfgKillRoute == "" && cfgKillRunCmd == "" && cfgKillNotifyUrl == "" {
		return
	}
	go func(ip string, port int) {
		if cfgKillRoute != "" {
			if err := runCmd(cfgKillRoute, *mode, ip, port); err != nil {
				logMain(false, "run kill_route:%s, host:%s:%d failed:%s", cfgKillRoute, ip, port, err.Error())
			}
		}

		if cfgKillRunCmd != "" {
			if err := runCmd(cfgKillRunCmd, *mode, ip, port); err != nil {
				logMain(false, "run kill_run_cmd:%s, host:%s:%d failed:%s", cfgKillRunCmd, ip, port, err.Error())
			}
		}

		if cfgKillNotifyUrl != "" {
			if err := requestUrl(cfgKillNotifyUrl, *mode, ip, port); err != nil {
				logMain(false, "notify kill_notify_url:%s, host:%s:%d failed:%s", cfgKillNotifyUrl, ip, port, err.Error())
			}
		}
	}(ip, port)
}

// tcp guard
func tcpGuard() {
	conn, err := net.ListenIP("ip4:tcp", &net.IPAddr{IP: serverIp})
	if err != nil {
		logMain(true, err.Error())
	}

	b := make([]byte, 1024)
	var tcp TCPHeader
	for {
		numRead, remoteAddr, err := conn.ReadFromIP(b)
		if err != nil {
			logMain(false, "read from ip:%s", err.Error())
			continue
		}
		NewTCPHeader(b[:numRead], &tcp)
		/*nmap: Page 65 of RFC 793 says that “if the [destination] port state is
		CLOSED .... an incoming segment not containing a RST causes a RST to be
		sent in response.”  Then the next page discusses packets sent to open
		ports without the SYN, RST, or ACK bits set, stating that: “you are
		unlikely to get here, but if you do, drop the segment, and return.”
		*/
		if tcp.HasFlag(RST) || tcp.HasFlag(ACK) {
			continue
		}

		port := int(tcp.Destination)
		ip := remoteAddr.IP
		ipString := ip.String()

		// is exclude port
		if isExlcudePort(port) {
			continue
		}

		// check ignore ip
		if isIgnoredIP(ip) {
			continue
		}

		// if blocked before
		if isBlockedIP(ipString) {
			continue
		}

		// verify port usage
		if smartVerify(port) {
			continue
		}

		logAlarm("attackalert: %s from host: %s to TCP port: %d",
			*reportPacketType(tcp.Ctrl), ipString, port)
		if checkStateEngine(ipString, port) {
			logBlocked("Host: %s Port: %d TCP Blocked", ipString, port)
			// run extern command
			runExternalCommand(ipString, port)
		}
	}
}

func udpGuard() {
	conn, err := net.ListenIP("ip4:udp", &net.IPAddr{IP: serverIp})
	if err != nil {
		logMain(true, err.Error())
	}

	b := make([]byte, 1024)
	var udp UDPHeader
	for {
		numRead, remoteAddr, err := conn.ReadFromIP(b)
		if err != nil {
			logMain(false, "read from ip:%s", err.Error())
			continue
		}
		NewUDPHeader(b[:numRead], &udp)
		port := int(udp.Destination)

		// ignore noisy port
		if _, ok := cfgNoisyPorts[port]; ok {
			continue
		}

		log.Printf("%v: %d->%d", remoteAddr, udp.Source, udp.Destination)
		ip := remoteAddr.IP
		ipString := ip.String()

		// is exclude port
		if isExlcudePort(port) {
			continue
		}

		// check ignore ip
		if isIgnoredIP(ip) {
			continue
		}

		// if blocked before
		if isBlockedIP(ipString) {
			continue
		}

		// verify port usage
		if smartVerify(port) {
			continue
		}

		logAlarm("attackalert: UDP scan from host: %s to UDP port: %d", ipString, port)
		if checkStateEngine(ipString, port) {
			logBlocked("Host: %s Port: %d UDP Blocked", ipString, port)
			// run extern command
			runExternalCommand(ipString, port)
		}
	}
}

func parseToken(line string) (token, value string) {
	line = strings.TrimRight(line, "\r\n")
	tokens := strings.SplitN(line, "=", 2)
	if len(tokens) != 2 {
		return
	}
	value = strings.TrimSpace(tokens[1])
	if value == "" {
		return
	}
	token = strings.TrimSpace(tokens[0])
	return
}

func parseInt(lineno int, token string, value string) int {
	v, err := strconv.Atoi(value)
	if err != nil {
		logMain(true, "line %d:%s, convert %s to int failed:%s", lineno, token, value, err.Error())
	}

	if v < 0 {
		logMain(true, "line %d:%s, invalid value:%d", lineno, token, v)
	}
	return v
}

func parseIp(lineno int, token string, value string) *net.IPNet {
	formalValue := value
	if !strings.Contains(value, "/") {
		formalValue = fmt.Sprintf("%s/%d", value, 32)
	}
	_, ipNet, err := net.ParseCIDR(formalValue)
	if err != nil {
		logMain(true, "line %d:%s, %s is not a legal CIDR notation ip address:%s", lineno, token, value, err.Error())
	}
	return ipNet
}

func parseFile(lineno int, token string, value string) io.Writer {
	f, err := os.OpenFile(value, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		logMain(true, "line %d:%s, open file %s failed:%s", lineno, token, value, err.Error())
	}
	return f
}

func readConfigFile(file string) {
	f, err := os.Open(file)
	if err != nil {
		logMain(true, "open file %s failed: %s", file, err.Error())
	}
	defer f.Close()

	rd := bufio.NewReader(f)
	lineno := 0
	for {
		line, err := rd.ReadString('\n')
		lineno++

		if !strings.HasPrefix(line, "#") {
			token, value := parseToken(line)
			switch token {
			case "min_port":
				cfgMinPort = parseInt(lineno, token, value)
			case "max_port":
				cfgMaxPort = parseInt(lineno, token, value)
			case "noisy_udp_port":
				port := parseInt(lineno, token, value)
				cfgNoisyPorts[port] = true
			case "exclude_port":
				port := parseInt(lineno, token, value)
				cfgExcludePorts[port] = true
			case "ignore_ip":
				ipNet := parseIp(lineno, token, value)
				cfgIgnoreIps = append(cfgIgnoreIps, ipNet)
			case "kill_route":
				cfgKillRoute = value
			case "kill_run_cmd":
				cfgKillRunCmd = value
			case "kill_notify_url":
				if _, err := url.Parse(value); err != nil {
					logMain(true, "line %d:%s, invalid url:%s", lineno, token, value)
				}
				cfgKillNotifyUrl = value
			case "scan_trigger":
				cfgScanTrigger = parseInt(lineno, token, value)
			case "alarm_log":
				cfgAlarmLogPath = value
				cfgAlarmLog = parseFile(lineno, token, value)
			case "blocked_log":
				cfgBlockedLogPath = value
				cfgBlockedLog = parseFile(lineno, token, value)
			default:
			}
		}
		if err != nil {
			break
		}
	}
}

func configGuard() {
	// add default ignore network
	defaultIgnoreNetwork := []string{
		"127.0.0.1/8",
	}

	for _, network := range defaultIgnoreNetwork {
		_, ipNet, err := net.ParseCIDR(network)
		if err != nil {
			log.Fatal(err)
		}
		cfgIgnoreIps = append(cfgIgnoreIps, ipNet)
	}

	// add local interface addresses to ignored list
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		logMain(true, "query system network interface addresses failed:%s", err.Error())
	}

	for _, addr := range addrs {
		if addr.Network() == "ip+net" {
			str := strings.Split(addr.String(), "/")[0]
			if ip := net.ParseIP(str); ip != nil {
				if ip = ip.To4(); ip != nil {
					if !isIgnoredIP(ip) {
						cfgIgnoreIps = append(cfgIgnoreIps, &net.IPNet{
							IP:   ip,
							Mask: net.CIDRMask(32, 32),
						})
					}
				}
			}
		}
	}

	// set logger
	if alarmLogger = createLogger(cfgAlarmLog); alarmLogger == nil {
		logMain(false, "WARNING no alarm log")
	}

	if blockedLogger = createLogger(cfgBlockedLog); blockedLogger == nil {
		logMain(false, "WARNING no blocked log")
	}
}

func configEcho() {
	logMain(false, "+++++++++++++ portguard started +++++++++++++")
	logMain(false, "+++++++++++++ config +++++++++++++")
	logMain(false, "+ debug: %v", *debug)
	logMain(false, "+ mode: %s", *mode)
	logMain(false, "+ monitor port range[%d, %d]", cfgMinPort, cfgMaxPort)
	var ports []string
	for port := range cfgExcludePorts {
		ports = append(ports, strconv.Itoa(port))
	}

	logMain(false, "+ exclude ports:%s", strings.Join(ports, ","))
	logMain(false, "+ ignore ip:")
	for _, network := range cfgIgnoreIps {
		logMain(false, "-%s", network.String())
	}
	logMain(false, "+ scan trigger:%d", cfgScanTrigger)
	logMain(false, "+ kill route:%q", cfgKillRoute)
	logMain(false, "+ kill run cmd:%q", cfgKillRunCmd)
	logMain(false, "+ kill notify url:%q", cfgKillNotifyUrl)
	logMain(false, "+ alarm log file:%q", cfgAlarmLogPath)
	logMain(false, "+ blocked log file:%q", cfgBlockedLogPath)
	logMain(false, "++++++++++++++++++ end ++++++++++++++++")
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s [configFile]\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(1)
}

func main() {

	mode = flag.String("m", "tcp", "portguard work mode: tcp or udp")
	debug = flag.Bool("d", false, "debug mode, print log to stderr")
	portCacheDuration = flag.Int64("duration", 120, "port cache duration")

	flag.Usage = usage
	flag.Parse()

	if *debug {
		mainLogger = log.New(io.Writer(os.Stderr), "", log.Ldate|log.Lmicroseconds)
	} else {
		var err error
		if mainLogger, err = syslog.NewLogger(syslog.LOG_ERR|syslog.LOG_LOCAL7, log.Ldate|log.Lmicroseconds); err != nil {
			logMain(true, "open syslog failed:%s", err.Error())
		}
	}

	args := flag.Args()
	if len(args) > 0 {
		readConfigFile(args[0])
	}
	configGuard()
	configEcho()

	if *mode == "tcp" {
		tcpGuard()
	} else if *mode == "udp" {
		udpGuard()
	} else {
		fmt.Fprintf(os.Stderr, "don't support mode: %s\n", *mode)
	}
}
