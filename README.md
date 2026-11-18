# Wi-Fi Print Bridge for macOS

Keep an older wired printer in use even if it has no Wi-Fi or AirPrint. Wi-Fi Print Bridge also helps when the printer and Wi-Fi clients are on different networks. A Mac connected to both networks advertises a printer on Wi-Fi and sends jobs over Ethernet. The bridge shares one selected printer at a time.

## Requirements

- An Apple silicon or Intel Mac running macOS 12 Monterey or later, with both Wi-Fi and Ethernet connected.
- Administrator access to install the background service.
- An Ethernet printer that accepts plain IPP and has a compatible print format. The Mac needs an Ethernet route to it and the printer needs a return route.
- A trusted Wi-Fi subnet. IPP and optional RAW printing use unencrypted connections.

## Install

1. Connect the Mac to the printer’s Ethernet network and the Wi-Fi network used by the phones or computers. Turn on the printer.
2. Open **Install Wi-Fi Print Bridge** and choose the printer in the list. Use **Rescan** after connecting a cable or starting the printer. If it does not appear, choose the manual address option and enter its IPv4 address or its full plain IPP URL. The full URL is needed if the printer uses a different port or path from `631/ipp/print`.
3. Leave **Allow Windows printing on this Wi-Fi network** selected if you want the optional RAW port for compatible Windows printer drivers. Clear it if you only need IPP or AirPrint.
4. Click **Install Bridge** and approve the administrator prompt. The installer checks the selected printer over Ethernet. The service starts after a graphical login when both interfaces are available and retries after interruptions. Keep the Mac awake while printing.

If macOS asks for Local Network access, allow it so the installer can discover devices. An ad hoc signed release has no Apple notarization ticket. If a managed Mac requires notarization, ask its administrator about locally approved software.

### Printer compatibility

| Printer capability | Bridge behavior |
| --- | --- |
| Native AirPrint with Apple Raster over IPP | Advertises AirPrint on Wi-Fi and forwards supported jobs to the printer. |
| Older printer that explicitly accepts PostScript over plain IPP | Advertises AirPrint and uses the Mac’s built-in CUPS PostScript converter. Tested input formats are Apple Raster (URF), PDF, JPEG, and PWG Raster. Printer-specific tray or finishing features may vary. |
| Compatible modern IPP formats and job operations | Offers an IPP queue for compatible Windows clients. Look for **Windows IPP: true** in the install details. |
| A compatible IPP printer that also accepts RAW TCP 9100 | Can forward RAW jobs from the Mac’s Wi-Fi subnet when enabled. Install a Windows driver that matches the physical printer. |

Printers that offer only RAW TCP, LPD, USB, authenticated IPP, or encrypted IPPS are outside this release. Ethernet discovery looks for IPP printers and a manual address still needs to lead to a usable plain IPP endpoint.

## Print from iPhone or iPad

Join the Wi-Fi network used by the Mac. Open a document, choose **Print**, then select **Ethernet Printer**. If the printer is missing, check that Wi-Fi allows device-to-device traffic and Bonjour discovery (UDP 5353). The Mac firewall may also need to allow incoming connections for `/Library/Application Support/WiFiPrintBridge/print-bridge`. [Apple’s AirPrint guide](https://support.apple.com/en-us/109349) · [Apple firewall settings](https://support.apple.com/guide/mac-help/change-firewall-settings-on-mac-mh11783/mac)

## Print from another Mac

Join the bridge Mac's Wi-Fi. In **System Settings → Printers & Scanners**, click **Add Printer, Scanner, or Fax**, choose **Ethernet Printer**, and select AirPrint if offered. For manual IPP setup, use the bridge Mac's Wi-Fi IPv4 address and this queue URL with the default port. [Apple’s guide to adding printers](https://support.apple.com/guide/mac-help/add-and-manage-printers-mh14004/mac)

```text
ipp://MAC_WIFI_IP:8631/ipp/print
```

You can also add the queue from Terminal. [CUPS lpadmin documentation](https://www.cups.org/doc/man-lpadmin.html)

```sh
lpadmin -p WiFiPrintBridge -E -v 'ipp://MAC_WIFI_IP:8631/ipp/print' -m everywhere
```

## Print from Windows

Join the Mac’s Wi-Fi network and use the Mac’s **Wi-Fi IPv4 address** shown by the installer.

- **IPP.** If the installer reports **Windows IPP: true**, the bridged printer has IPP 2.x, modern document formats and the required job operations. Add the printer through **Settings → Bluetooth & devices → Printers & scanners**. On Windows versions that provide the `IppURL` option for `Add-Printer`, you can also use the following in PowerShell after substituting the Mac’s address. [Microsoft Add-Printer documentation](https://learn.microsoft.com/en-us/powershell/module/printmanagement/add-printer?view=windowsserver2025-ps)

  ```powershell
  Add-Printer -Name "Ethernet Printer" -IppURL "http://MAC_WIFI_IP:8631/ipp/print"
  ```

- **RAW.** If the installer reports that RAW printing is available, manually add a **Standard TCP/IP Port** for the Mac’s Wi-Fi address. Set protocol **RAW**, port **9100**, and clear **SNMP Status Enabled**. Select the physical printer’s Windows driver and print a test page. The bridge forwards the driver-generated bytes to the printer. A managed PC may restrict vendor drivers. [Microsoft Standard TCP/IP Port Monitor](https://learn.microsoft.com/en-us/troubleshoot/windows-server/printing/standard-port-monitor-for-tcpip) · [Windows Protected Print Mode](https://learn.microsoft.com/en-us/windows/modern-print/windows-protected-print-mode/windows-protected-print-mode)

## Ethernet routing

**Use current routing** is appropriate when Ethernet already reaches the printer. **Network Settings** lets you choose a different network setup if necessary.

- **Router between the Mac and printer.** Enter the router’s address on the Mac’s Ethernet subnet. The installer adds a route for the printer scoped to Ethernet.
- **Same Ethernet segment, separate IP subnets.** If the printer is on the same switch or cable, enter a reserved address from the printer’s subnet that is unused on both networks. The installer adds that address as an Ethernet alias and scopes the printer route to Ethernet.
- **Several network adapters.** Enter the matching adapter’s current IP in the installer, or specify the interface name with the command-line options. You can list hardware ports with `networksetup -listallhardwareports`.

## Terminal use

The app includes the command-line installer in `Contents/Resources/bridge`. Run these commands from that directory. Replace the documentation address and interface with the values returned by discovery.

```sh
bash install.sh discover
PRINTER_URI='ipp://192.0.2.25:631/ipp/print'
bash install.sh diagnose --backend "$PRINTER_URI" --ethernet-interface en1
sudo bash install.sh install --backend "$PRINTER_URI" --ethernet-interface en1 --windows-lan on
sudo bash install.sh status
```

Discovery and diagnosis make no network configuration changes. To restrict RAW access to one client instead of enabling the Wi-Fi subnet, pass `--windows-client CLIENT_IP`. To disable RAW, run `sudo bash install.sh install --windows-lan off`. Run `bash install.sh --help` for the complete options.

## Update and remove

Open the new installer and choose **Use the installed printer** to update the software while retaining the selected printer. The checkbox controls optional Windows RAW access. The Terminal command `sudo bash install.sh install` with no options preserves both printer and Windows settings. Selecting another printer is supported in place when the existing configuration has no custom route that would need to change.

The update stops the old process and starts the replacement under the same single launchd label, `local.wifiprintbridge`. To verify the active service and see its configured command and PID, run `sudo launchctl print system/local.wifiprintbridge`. From the app’s bridge directory, `sudo bash install.sh status` also shows the current settings and recent service log.

To remove the service, run `sudo bash install.sh uninstall` from the app’s bridge directory. This stops the service and removes the printer route or Ethernet alias if the bridge added one.

## Build from source

Use macOS, the Xcode Command Line Tools, and Go 1.26 to retain macOS 12 support. [Go 1.26 compatibility](https://go.dev/doc/go1.26)

```sh
bash build.sh
```

The script builds and verifies a universal **build/Install Wi-Fi Print Bridge.app**. Set `GO` to the absolute path to a Go 1.26 executable if it is outside your default path. The app is ad hoc signed by default. To use a Developer ID certificate already in your keychain, set `CODESIGN_IDENTITY` to the certificate identity when invoking `bash build.sh`. Notarization is separate.

Run the Go tests and both local print-path smoke tests from the source checkout:

```sh
(cd bridge/src && go test ./...)
bash tests/smoke.sh 'build/Install Wi-Fi Print Bridge.app/Contents/Resources/bridge/print-bridge'
bash tests/legacy-smoke.sh 'build/Install Wi-Fi Print Bridge.app/Contents/Resources/bridge/print-bridge'
```

Both smoke tests use loopback printer fixtures. You can review dependency notices in `licenses/`.

## License

Wi-Fi Print Bridge is available under the [MIT License](LICENSE). See [licenses](licenses/) for third-party notices.
