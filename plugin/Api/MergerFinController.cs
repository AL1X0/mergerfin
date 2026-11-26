using System;
using System.IO;
using System.Linq;
using System.Net.Http;
using System.Security.Claims;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Models;
using Jellyfin.Plugin.MergerFin.Services;
using MediaBrowser.Controller.Library;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Api;

/// <summary>
/// MergerFin plugin API controller: admin management endpoints + authenticated stream proxy/redirect.
/// </summary>
[ApiController]
[Route("MergerFin")]
public class MergerFinController : ControllerBase
{
    private readonly UpstreamClient _client;
    private readonly RemoteCatalogStore _store;
    private readonly CatalogSyncService _syncService;
    private readonly IUserManager _userManager;
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<MergerFinController> _logger;

    public MergerFinController(
        UpstreamClient client,
        RemoteCatalogStore store,
        CatalogSyncService syncService,
        IUserManager userManager,
        IHttpClientFactory httpClientFactory,
        ILogger<MergerFinController> logger)
    {
        _client = client;
        _store = store;
        _syncService = syncService;
        _userManager = userManager;
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    public class TestServerRequest
    {
        public string BaseUrl { get; set; } = string.Empty;

        public string ApiKey { get; set; } = string.Empty;
    }

    [HttpPost("Servers/Test")]
    [Authorize(Policy = "RequiresElevation")]
    public async Task<ActionResult<ProbeResult>> TestServer([FromBody] TestServerRequest req, CancellationToken ct)
    {
        var result = await _client.TestApiKeyAsync(req.BaseUrl, req.ApiKey, ct).ConfigureAwait(false);
        return Ok(result);
    }

    [HttpGet("Servers/{serverId}/Libraries")]
    [Authorize(Policy = "RequiresElevation")]
    public async Task<ActionResult<System.Collections.Generic.List<RemoteLibraryInfo>>> GetServerLibraries([FromRoute] string serverId, CancellationToken ct)
    {
        var cfg = Plugin.Instance?.Configuration;
        var srv = cfg?.Servers.FirstOrDefault(s => string.Equals(s.Id, serverId, StringComparison.OrdinalIgnoreCase));
        if (srv == null)
        {
            return NotFound(new { error = "Server not found." });
        }

        try
        {
            var folders = await _client.FetchVirtualFoldersAsync(srv, ct).ConfigureAwait(false);
            var list = folders.Select(f => new RemoteLibraryInfo
            {
                Id = f.ItemId,
                Name = f.Name,
                CollectionType = string.IsNullOrWhiteSpace(f.CollectionType) ? "mixed" : f.CollectionType,
                AutoTarget = CatalogSyncService.DetectDefaultTargetFolder(f.Name, f.CollectionType)
            }).ToList();

            return Ok(list);
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "MergerFin failed listing libraries on server {Server}", srv.Name);
            return StatusCode(StatusCodes.Status502BadGateway, new { error = ex.Message });
        }
    }

    [HttpPost("Sync")]
    [Authorize(Policy = "RequiresElevation")]
    public ActionResult TriggerSync()
    {
        if (_syncService.IsRunning)
        {
            return Ok(new { started = false, running = true, message = "Sync already in progress." });
        }

        _ = Task.Run(async () =>
        {
            try
            {
                await _syncService.SyncAllAsync(null, CancellationToken.None).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "MergerFin background sync failed");
            }
        });

        return Accepted(new { started = true, message = "Sync started." });
    }

    [HttpGet("Status")]
    [Authorize(Policy = "RequiresElevation")]
    public ActionResult<PluginStatusResponse> GetStatus()
    {
        var (total, movies, episodes, lastSyncUtc, lastError) = _store.GetStats();
        return Ok(new PluginStatusResponse
        {
            Running = _syncService.IsRunning,
            TotalSources = total,
            Movies = movies,
            Episodes = episodes,
            LastSyncUtc = lastSyncUtc == default ? null : lastSyncUtc,
            LastError = lastError
        });
    }

    [HttpGet("Stream/{sourceId}")]
    [HttpHead("Stream/{sourceId}")]
    [AllowAnonymous]
    public async Task StreamRemoteSource([FromRoute] string sourceId, CancellationToken ct)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null)
        {
            Response.StatusCode = StatusCodes.Status503ServiceUnavailable;
            return;
        }

        // This endpoint must stay anonymous for Jellyfin's own ffmpeg / HTTP media pipeline (it calls the
        // .strm / mf_* loopback URL without credentials), but outside callers must be authenticated users.
        var isLocalCaller = IsLocalCaller(HttpContext);
        if (!isLocalCaller && User?.Identity?.IsAuthenticated != true)
        {
            Response.StatusCode = StatusCodes.Status401Unauthorized;
            return;
        }

        var src = _store.GetBySourceId(sourceId);
        if (src == null)
        {
            Response.StatusCode = StatusCodes.Status404NotFound;
            await Response.WriteAsync("Remote source not found.", ct).ConfigureAwait(false);
            return;
        }

        var srv = cfg.Servers.FirstOrDefault(s => s.Enabled && string.Equals(s.Id, src.ServerId, StringComparison.OrdinalIgnoreCase));
        if (srv == null)
        {
            Response.StatusCode = StatusCodes.Status502BadGateway;
            await Response.WriteAsync("Upstream server disabled or missing.", ct).ConfigureAwait(false);
            return;
        }

        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(apiKey))
        {
            Response.StatusCode = StatusCodes.Status502BadGateway;
            await Response.WriteAsync("Upstream server API key missing.", ct).ConfigureAwait(false);
            return;
        }

        var (localUserId, localUsername) = ResolveCallerUser();
        var identity = new UpstreamSessionIdentity
        {
            LocalUsername = localUsername,
            ClientName = "Jellyfin",
            DeviceName = Request.Headers.UserAgent.ToString(),
            DeviceId = localUserId ?? string.Empty
        };

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var targetUrl =
            $"{baseUrl}/Videos/{Uri.EscapeDataString(src.RemoteItemId)}/stream" +
            $"?static=true&mediaSourceId={Uri.EscapeDataString(src.RemoteMediaSourceId)}&ApiKey={Uri.EscapeDataString(apiKey)}";

        // The redirect target embeds the remote server's admin API key: only ever hand it to this Jellyfin
        // server itself (which follows the redirect internally). Outside callers are always proxied.
        if (isLocalCaller && string.Equals(srv.StreamMode, "redirect", StringComparison.OrdinalIgnoreCase))
        {
            Response.Redirect(targetUrl, permanent: false);
            return;
        }

        using var upReq = new HttpRequestMessage(
            HttpMethods.IsHead(Request.Method) ? HttpMethod.Head : HttpMethod.Get,
            targetUrl);
        upReq.Headers.TryAddWithoutValidation("X-Emby-Token", apiKey);
        upReq.Headers.TryAddWithoutValidation("Authorization", UpstreamClient.BuildMediaBrowserHeader(apiKey, identity));

        if (Request.Headers.TryGetValue("Range", out var rangeVal) && !string.IsNullOrWhiteSpace(rangeVal))
        {
            upReq.Headers.TryAddWithoutValidation("Range", rangeVal.ToString());
        }

        if (Request.Headers.TryGetValue("If-Range", out var ifRangeVal) && !string.IsNullOrWhiteSpace(ifRangeVal))
        {
            upReq.Headers.TryAddWithoutValidation("If-Range", ifRangeVal.ToString());
        }

        var httpClient = _httpClientFactory.CreateClient("MergerFinStream");
        httpClient.Timeout = Timeout.InfiniteTimeSpan;

        using var upResp = await httpClient.SendAsync(upReq, HttpCompletionOption.ResponseHeadersRead, ct).ConfigureAwait(false);

        Response.StatusCode = (int)upResp.StatusCode;
        CopyHeader(upResp, "Content-Type");
        CopyHeader(upResp, "Content-Length");
        CopyHeader(upResp, "Content-Range");
        CopyHeader(upResp, "Accept-Ranges");
        CopyHeader(upResp, "ETag");
        CopyHeader(upResp, "Last-Modified");

        if ((Response.StatusCode == 200 || Response.StatusCode == 206) && !Response.Headers.ContainsKey("Accept-Ranges"))
        {
            Response.Headers["Accept-Ranges"] = "bytes";
        }

        if (HttpMethods.IsHead(Request.Method))
        {
            return;
        }

        try
        {
            await using var bodyStream = await upResp.Content.ReadAsStreamAsync(ct).ConfigureAwait(false);
            await bodyStream.CopyToAsync(Response.Body, 65536, ct).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            // Normal when client aborts an open Range stream during a timeline seek.
        }
        catch (IOException) when (ct.IsCancellationRequested)
        {
            // Normal client disconnect during seek.
        }
    }

    private static bool IsLocalCaller(HttpContext ctx)
    {
        var remote = ctx.Connection.RemoteIpAddress;
        if (remote == null)
        {
            // In-process calls (e.g. TestServer) have no remote endpoint.
            return true;
        }

        if (remote.IsIPv4MappedToIPv6)
        {
            remote = remote.MapToIPv4();
        }

        if (System.Net.IPAddress.IsLoopback(remote))
        {
            return true;
        }

        // Same host calling itself on a non-loopback address (e.g. LocalBaseUrl = http://jellyfin:8096 in Docker).
        var local = ctx.Connection.LocalIpAddress;
        if (local != null && local.IsIPv4MappedToIPv6)
        {
            local = local.MapToIPv4();
        }

        return local != null && remote.Equals(local);
    }

    private (string? UserId, string Username) ResolveCallerUser()
    {
        var claimUid = User?.FindFirstValue("Jellyfin-UserId") ?? User?.FindFirstValue(ClaimTypes.NameIdentifier);
        if (Guid.TryParse(claimUid, out var parsedGuid))
        {
            var u = _userManager.GetUserById(parsedGuid);
            if (u != null)
            {
                return (u.Id.ToString("N"), u.Username);
            }
        }

        if (Request.Query.TryGetValue("userId", out var qUid) && Guid.TryParse(qUid.ToString(), out var qGuid))
        {
            var u = _userManager.GetUserById(qGuid);
            if (u != null)
            {
                return (u.Id.ToString("N"), u.Username);
            }
        }

        return (null, string.Empty);
    }

    private void CopyHeader(HttpResponseMessage src, string headerName)
    {
        if (src.Headers.TryGetValues(headerName, out var vals))
        {
            Response.Headers[headerName] = vals.ToArray();
        }
        else if (src.Content.Headers.TryGetValues(headerName, out var cVals))
        {
            Response.Headers[headerName] = cVals.ToArray();
        }
    }
}
