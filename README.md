# MergerFin

[![Jellyfin](https://img.shields.io/badge/Jellyfin-12.4%2B-00A4DC?logo=jellyfin&logoColor=white)](https://jellyfin.org)
[![Plugin Version](https://img.shields.io/badge/Plugin-v0.1.16.0-blue)](manifest.json)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)

**MergerFin** is an open-source native Jellyfin plugin that federates one or more remote Jellyfin servers directly into your existing Jellyfin server.

Instead of switching between servers in your media apps or running an external proxy, MergerFin integrates remote catalogs natively inside your Jellyfin instance with multi-version deduplication, optional remote-only `.strm` library indexing, and automatic watch-state synchronization.

---

## Installation (Plugin Repository)

Add the MergerFin repository URL to your Jellyfin server:

```text
https://raw.githubusercontent.com/AL1X0/mergerfin/main/manifest.json
```

### Step-by-Step

1. Open your Jellyfin web interface and go to **Dashboard** → **Plugins** → **Repositories** (or **Catalog** → ⚙️ **Repositories**).
2. Click **Add** (`+`) and fill in:
   - **Repository Name**: `MergerFin`
   - **Repository URL**: `https://raw.githubusercontent.com/AL1X0/mergerfin/main/manifest.json`
3. Click **Save**, then switch to the **Catalog** tab.
4. Locate **MergerFin** (under **General**), select the latest version, and click **Install**.
5. **Restart your Jellyfin server** to load the plugin.

---

## How It Works

### 1. Native Multi-Version Injection (`IMediaSourceProvider`)
MergerFin indexes movies and episodes from your configured remote Jellyfin servers and matches them to your local library via external provider IDs (`TMDB`, `TVDB`, `IMDb`). When a title exists on both your local server and one or more remote servers, MergerFin injects the remote versions directly into Jellyfin's native **Version** selector:

```text
[Server A] 2160p HEVC (25.0 Mbps)
[Server B] 1080p H264 (8.0 Mbps)
```

### 2. Optional Remote-Only `.strm` Library (Movies / Shows / Anime)
If you also want to browse movies, TV shows, and anime that exist **only** on your remote servers, set a **Remote-Only `.strm` Output Directory** in the plugin settings. MergerFin reads each remote server's Jellyfin libraries (`GET /Library/VirtualFolders`) and automatically organizes lightweight `.strm` files into `Movies/`, `Shows/`, and `Anime/` subfolders (libraries containing `anime`, `animé`, or `japanim` are routed to `Anime/` automatically), cleans up stale files and empty folders, backfills missing runtimes (`RunTimeTicks`) for Continue Watching, and fixes `PlaybackInfo` metadata (real remote file size, bitrate, and accurate timeline seeking).

You can also click **Libraries** on any configured remote server to override the target folder per library (`Auto`, `Movies`, `Shows`, `Anime`, a custom subfolder name, or **`Ignore`** to skip a library completely).

### 3. Streaming Modes: `proxy` vs `redirect`
Each remote server can be configured independently:
- **`proxy` (default)**: Streams media through your local Jellyfin server (`/MergerFin/Stream/{sourceId}`) with full HTTP `Range` / `206 Partial Content` support, keeping remote API keys hidden from clients.
- **`redirect`**: Returns an `HTTP 302` redirect directly to the remote Jellyfin server.

### 4. API Key Mode & Automatic Watch-State Sync
- Remote servers authenticate with a single Jellyfin **API Key** — no per-user credentials or manual account mapping required.
- When a local user watches, pauses, seeks, completes, toggles played status, or favorites a remote item, MergerFin relays live playback status to the active remote server and syncs watch state to matching remote users automatically by **username**.

### 5. Mesh Anti-Loop Protection
Two or more Jellyfin servers running MergerFin can safely federate each other: MergerFin automatically ignores remote MergerFin `.strm` stubs, relayed `mf_*` sources, and relayed playback sessions so servers only share their own local files without catalog or playback echo loops.

---

## Configuration

### 1. Generate an API Key on Each Remote Server
On each remote Jellyfin server you want to connect:
1. Open **Dashboard** → **API Keys**.
2. Click `+`, name the key (for example, `MergerFin`), and copy the generated key.

### 2. Add Remote Servers
On your local Jellyfin server, open **Dashboard** → **Plugins** → **MergerFin**:
1. Under **Remote Jellyfin Servers**, fill in:
   - **Display Name**: Label shown in the version selector (e.g. `[RemoteServer] 1080p H264`).
   - **Server Base URL**: Base URL of the remote Jellyfin server (e.g. `https://jellyfin.example.com`).
   - **Jellyfin API Key**: API key generated in Step 1.
   - **Priority**: Lower number = higher priority (default `100`).
   - **Stream Mode**: `proxy` or `redirect`.
2. Click **Test & Add Server** to verify connectivity and save.
3. You can later adjust **Priority**, **Stream Mode**, and **Enabled** inline, click **Libraries** to map or ignore individual remote libraries (`Movies`, `Shows`, `Anime`, custom folder, or `Ignore`), or click **Edit** to update server details.

### 3. General Settings

| Setting | Default | Description |
| --- | --- | --- |
| **Remote-Only `.strm` Output Directory** | *(empty)* | Optional folder where MergerFin writes `.strm` stubs (`Movies/`, `Shows/`, `Anime/`, or custom subfolders) for remote titles. Add each subfolder to its corresponding local Jellyfin library. Leave empty to only inject extra versions onto existing local items. |
| **Local Jellyfin Base URL** | `http://127.0.0.1:8096` | Internal URL used for `/MergerFin/Stream/{id}` links inside `.strm` files and injected media sources. |
| **Inject remote versions onto matching local movies & episodes** | Enabled | Attaches matching remote sources to local items via `IMediaSourceProvider`. |
| **Skip `.strm` files for items already present locally** | Enabled | Prevents duplicate `.strm` entries when merging into existing libraries. Disable this if you use a dedicated standalone library for all remote content. |
| **Sync local playback, played & favorite state back to remote servers** | Enabled | Syncs live playback progress, resume position, played status, and favorites to remote users matched by username. |
| **Tag movies & series with their source server name(s)** | Enabled | Adds native Jellyfin Tags (e.g. `Source: BelkaFlix`) to movies and series so you can filter libraries by origin server. |
| **Show source server banner on Movie & Series pages** | Enabled | Displays the source server banner (`Source: BelkaFlix`) right above the synopsis on Movie and Series pages across all Jellyfin clients (including Jellyfin Web, which does not render Jellyfin Tags on detail pages). |
| **Server Tag Prefix** | `Source: ` | Optional prefix prepended to server origin tags. Leave blank to tag with the bare server name. |
| **Local Media Tag** | *(empty)* | Optional tag added to movies and series also stored locally (e.g. `Local`). Leave blank to only tag remote servers. |

### 4. Sync Your Catalog
- Click **Sync Now** at the top of the MergerFin settings page to run an immediate sync.
- MergerFin also registers a native Jellyfin scheduled task (**Dashboard → Scheduled Tasks → MergerFin → Sync MergerFin Remote Servers**) that runs automatically every 30 minutes.
- If you enabled `.strm` output, run a Jellyfin library scan so the new `Movies/` and `Shows/` folders are indexed.

---

## Compatibility

- **Jellyfin Server**: `12.4.0+` (`targetAbi: 12.4.0.0`)
- **Clients**: Compatible with all native Jellyfin clients (Jellyfin Web, Jellyfin Media Player, Swiftfin, Infuse, Streamyfin, Findroid, Android TV, Kodi) since remote sources are served directly through Jellyfin's native APIs.

---

## License

MergerFin is licensed under the **GNU General Public License v3.0 (GPL-3.0)**. See [LICENSE](LICENSE) for full details.
