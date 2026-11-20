using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Linq;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Configuration;
using Jellyfin.Plugin.MergerFin.Models;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// HTTP client for communicating with remote Jellyfin servers (auth, catalog fetch, streaming, watch-state).
/// </summary>
public class UpstreamClient
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = true
    };

    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<UpstreamClient> _logger;

    public UpstreamClient(IHttpClientFactory httpClientFactory, ILogger<UpstreamClient> logger)
    {
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    public static string BuildMediaBrowserHeader(string accessToken, UpstreamSessionIdentity? id = null)
    {
        if (id == null || (string.IsNullOrWhiteSpace(id.LocalUsername) && string.IsNullOrWhiteSpace(id.ClientName)))
        {
            const string baseHdr = "MediaBrowser Client=\"MergerFin Plugin\", Device=\"Jellyfin Host\", DeviceId=\"mergerfin-plugin\", Version=\"0.1.0\"";
            return string.IsNullOrEmpty(accessToken) ? baseHdr : $"{baseHdr}, Token=\"{Sanitize(accessToken, 256)}\"";
        }

        var client = string.IsNullOrWhiteSpace(id.ClientName) ? "MergerFin Plugin" : Sanitize(id.ClientName, 48);
        var user = Sanitize(id.LocalUsername, 48);
        var device = Sanitize(id.DeviceName, 48);

        string displayDevice;
        if (!id.IsMappedUser && !string.IsNullOrEmpty(user))
        {
            displayDevice = string.IsNullOrEmpty(device) ? user : $"{user} ({device})";
        }
        else
        {
            displayDevice = string.IsNullOrEmpty(device) ? "Jellyfin Host" : device;
        }

        var devId = BuildDeviceId(user, id.DeviceId);
        var header = $"MediaBrowser Client=\"{client}\", Device=\"{displayDevice}\", DeviceId=\"{devId}\", Version=\"0.1.0\"";
        if (!string.IsNullOrEmpty(accessToken))
        {
            header += $", Token=\"{Sanitize(accessToken, 256)}\"";
        }

        return header;
    }

    private static string BuildDeviceId(string user, string rawDeviceId)
    {
        var u = SanitizeToken(user, 24);
        var d = SanitizeToken(rawDeviceId, 32);
        if (!string.IsNullOrEmpty(u) && !string.IsNullOrEmpty(d))
        {
            return $"mf-{u}-{d}";
        }

        if (!string.IsNullOrEmpty(u))
        {
            return $"mf-{u}";
        }

        return !string.IsNullOrEmpty(d) ? $"mf-{d}" : "mergerfin-plugin";
    }

    private static string Sanitize(string? value, int maxLen)
    {
        if (string.IsNullOrWhiteSpace(value))
        {
            return string.Empty;
        }

        var sb = new StringBuilder(Math.Min(value.Length, maxLen));
        foreach (var c in value.Trim())
        {
            if (c < 0x20 || c == 0x7f || c == '"' || c == '\\')
            {
                continue;
            }

            sb.Append(c);
            if (sb.Length >= maxLen)
            {
                break;
            }
        }

        return sb.ToString().Trim();
    }

    private static string SanitizeToken(string? value, int maxLen)
    {
        if (string.IsNullOrWhiteSpace(value))
        {
            return string.Empty;
        }

        var sb = new StringBuilder(Math.Min(value.Length, maxLen));
        foreach (var c in value.Trim().ToLowerInvariant())
        {
            if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_')
            {
                sb.Append(c);
                if (sb.Length >= maxLen)
                {
                    break;
                }
            }
        }

        return sb.ToString().Trim('-', '_');
    }

    public async Task<ProbeResult> TestConnectionAsync(string baseUrl, string username, string password, CancellationToken ct)
    {
        var cleanUrl = (baseUrl ?? string.Empty).Trim().TrimEnd('/');
        if (string.IsNullOrWhiteSpace(cleanUrl))
        {
            return new ProbeResult { Online = false, Error = "BaseUrl is required." };
        }

        var client = _httpClientFactory.CreateClient("MergerFin");
        client.Timeout = TimeSpan.FromSeconds(15);

        var sw = Stopwatch.StartNew();
        try
        {
            using var infoResp = await client.GetAsync($"{cleanUrl}/System/Info/Public", ct).ConfigureAwait(false);
            sw.Stop();
            if (!infoResp.IsSuccessStatusCode)
            {
                return new ProbeResult
                {
                    Online = false,
                    LatencyMs = sw.ElapsedMilliseconds,
                    Error = $"/System/Info/Public returned HTTP {(int)infoResp.StatusCode}"
                };
            }

            var infoJson = await infoResp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            using var infoDoc = JsonDocument.Parse(infoJson);
            var serverName = infoDoc.RootElement.TryGetProperty("ServerName", out var sn) ? sn.GetString() ?? string.Empty : string.Empty;
            var version = infoDoc.RootElement.TryGetProperty("Version", out var vr) ? vr.GetString() ?? string.Empty : string.Empty;

            var payload = JsonSerializer.Serialize(new { Username = username, Pw = password });
            using var authReq = new HttpRequestMessage(HttpMethod.Post, $"{cleanUrl}/Users/AuthenticateByName");
            authReq.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(string.Empty));
            authReq.Content = new StringContent(payload, Encoding.UTF8, "application/json");

            using var authResp = await client.SendAsync(authReq, ct).ConfigureAwait(false);
            if (!authResp.IsSuccessStatusCode)
            {
                return new ProbeResult
                {
                    Online = true,
                    LatencyMs = sw.ElapsedMilliseconds,
                    ServerName = serverName,
                    Version = version,
                    Error = $"Authentication failed (HTTP {(int)authResp.StatusCode})"
                };
            }

            var authJson = await authResp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            using var authDoc = JsonDocument.Parse(authJson);
            var token = authDoc.RootElement.TryGetProperty("AccessToken", out var tk) ? tk.GetString() ?? string.Empty : string.Empty;
            var remoteUserId = authDoc.RootElement.TryGetProperty("User", out var usr) && usr.TryGetProperty("Id", out var uid)
                ? uid.GetString() ?? string.Empty
                : string.Empty;

            return new ProbeResult
            {
                Online = true,
                LatencyMs = sw.ElapsedMilliseconds,
                ServerName = serverName,
                Version = version,
                RemoteUserId = remoteUserId,
                AccessToken = token
            };
        }
        catch (Exception ex)
        {
            sw.Stop();
            return new ProbeResult
            {
                Online = false,
                LatencyMs = sw.ElapsedMilliseconds,
                Error = ex.Message
            };
        }
    }

    public async Task<List<RemoteItemDto>> FetchAllItemsAsync(RemoteServerConfig srv, string[] itemTypes, CancellationToken ct)
    {
        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var client = _httpClientFactory.CreateClient("MergerFin");
        client.Timeout = TimeSpan.FromSeconds(45);

        const int pageSize = 200;
        var all = new List<RemoteItemDto>();
        var startIndex = 0;
        var typesParam = Uri.EscapeDataString(string.Join(",", itemTypes));
        const string fields = "ProviderIds,MediaSources,MediaStreams,ProductionYear";

        while (true)
        {
            var path = string.IsNullOrWhiteSpace(srv.RemoteUserId)
                ? $"{baseUrl}/Items"
                : $"{baseUrl}/Users/{Uri.EscapeDataString(srv.RemoteUserId)}/Items";
            var url = $"{path}?Recursive=true&IncludeItemTypes={typesParam}&CollapseBoxSetItems=false&Fields={fields}&StartIndex={startIndex}&Limit={pageSize}";

            using var req = new HttpRequestMessage(HttpMethod.Get, url);
            req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(srv.AccessToken));
            req.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
            resp.EnsureSuccessStatusCode();

            var body = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            var page = JsonSerializer.Deserialize<RemoteItemsResponse>(body, JsonOptions);
            if (page == null || page.Items.Count == 0)
            {
                break;
            }

            all.AddRange(page.Items);
            startIndex += page.Items.Count;
            if (page.TotalRecordCount > 0 && all.Count >= page.TotalRecordCount)
            {
                break;
            }

            if (page.Items.Count < pageSize)
            {
                break;
            }
        }

        return all;
    }

    public (string RemoteUserId, string AccessToken, bool IsMapped) ResolveCredentials(
        PluginConfiguration cfg,
        RemoteServerConfig srv,
        string? localUserId)
    {
        if (!string.IsNullOrWhiteSpace(localUserId))
        {
            var normLocal = localUserId.Replace("-", string.Empty, StringComparison.Ordinal).ToLowerInvariant();
            var mapping = cfg.UserMappings.FirstOrDefault(m =>
                string.Equals(m.ServerId, srv.Id, StringComparison.OrdinalIgnoreCase) &&
                string.Equals(m.LocalUserId.Replace("-", string.Empty, StringComparison.Ordinal), normLocal, StringComparison.OrdinalIgnoreCase) &&
                !string.IsNullOrWhiteSpace(m.AccessToken));
            if (mapping != null)
            {
                return (mapping.RemoteUserId, mapping.AccessToken, true);
            }
        }

        return (srv.RemoteUserId, srv.AccessToken, false);
    }

    public async Task MarkPlayedAsync(
        RemoteServerConfig srv,
        string remoteUserId,
        string accessToken,
        string remoteItemId,
        bool played,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        if (string.IsNullOrWhiteSpace(remoteUserId) || string.IsNullOrWhiteSpace(accessToken))
        {
            return;
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var url = $"{baseUrl}/Users/{Uri.EscapeDataString(remoteUserId)}/PlayedItems/{Uri.EscapeDataString(remoteItemId)}";
        using var req = new HttpRequestMessage(played ? HttpMethod.Post : HttpMethod.Delete, url);
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(accessToken, identity));

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin MarkPlayed upstream failed for server {Server}", srv.Name);
        }
    }

    public async Task MarkFavoriteAsync(
        RemoteServerConfig srv,
        string remoteUserId,
        string accessToken,
        string remoteItemId,
        bool favorite,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        if (string.IsNullOrWhiteSpace(remoteUserId) || string.IsNullOrWhiteSpace(accessToken))
        {
            return;
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var url = $"{baseUrl}/Users/{Uri.EscapeDataString(remoteUserId)}/FavoriteItems/{Uri.EscapeDataString(remoteItemId)}";
        using var req = new HttpRequestMessage(favorite ? HttpMethod.Post : HttpMethod.Delete, url);
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(accessToken, identity));

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin MarkFavorite upstream failed for server {Server}", srv.Name);
        }
    }

    public async Task ReportPlaystateAsync(
        RemoteServerConfig srv,
        string accessToken,
        string endpointPath,
        object body,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        if (string.IsNullOrWhiteSpace(accessToken))
        {
            return;
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var url = $"{baseUrl}{endpointPath}";
        using var req = new HttpRequestMessage(HttpMethod.Post, url);
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(accessToken, identity));
        req.Content = new StringContent(JsonSerializer.Serialize(body), Encoding.UTF8, "application/json");

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin ReportPlaystate {Path} failed for server {Server}", endpointPath, srv.Name);
        }
    }
}
