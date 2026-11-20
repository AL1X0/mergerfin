using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Models;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Entities.Movies;
using MediaBrowser.Controller.Entities.TV;
using MediaBrowser.Controller.Library;
using MediaBrowser.Controller.Session;
using MediaBrowser.Model.Entities;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// Listens to local Jellyfin playback & watch-state events and propagates them cleanly
/// to remote Jellyfin servers with strict loop-prevention for meshed MergerFin setups.
/// </summary>
public class WatchStateSyncService : IHostedService
{
    private static readonly TimeSpan UserDataCooldown = TimeSpan.FromSeconds(15);

    private readonly ISessionManager _sessionManager;
    private readonly IUserDataManager _userDataManager;
    private readonly IUserManager _userManager;
    private readonly RemoteCatalogStore _store;
    private readonly UpstreamClient _client;
    private readonly ILogger<WatchStateSyncService> _logger;

    // Tracks recent UserData propagations to prevent echo loops between meshed servers.
    private readonly ConcurrentDictionary<string, DateTime> _recentUserData = new(StringComparer.OrdinalIgnoreCase);

    public WatchStateSyncService(
        ISessionManager sessionManager,
        IUserDataManager userDataManager,
        IUserManager userManager,
        RemoteCatalogStore store,
        UpstreamClient client,
        ILogger<WatchStateSyncService> logger)
    {
        _sessionManager = sessionManager;
        _userDataManager = userDataManager;
        _userManager = userManager;
        _store = store;
        _client = client;
        _logger = logger;
    }

    public Task StartAsync(CancellationToken cancellationToken)
    {
        _sessionManager.PlaybackStart += OnPlaybackStart;
        _sessionManager.PlaybackStopped += OnPlaybackStopped;
        _userDataManager.UserDataSaved += OnUserDataSaved;
        return Task.CompletedTask;
    }

    public Task StopAsync(CancellationToken cancellationToken)
    {
        _sessionManager.PlaybackStart -= OnPlaybackStart;
        _sessionManager.PlaybackStopped -= OnPlaybackStopped;
        _userDataManager.UserDataSaved -= OnUserDataSaved;
        return Task.CompletedTask;
    }

    private static bool IsRelayedOrSystemSession(SessionInfo? session)
    {
        if (session == null || session.UserId == Guid.Empty)
        {
            // API-key sessions have UserId == Guid.Empty ("User null").
            // Never re-relay them or two meshed MergerFin servers will ping-pong infinitely!
            return true;
        }

        var devId = session.DeviceId ?? string.Empty;
        if (devId.StartsWith("mf-", StringComparison.OrdinalIgnoreCase) ||
            devId.Contains("mergerfin", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        var client = session.Client ?? string.Empty;
        if (client.Contains("mergerfin", StringComparison.OrdinalIgnoreCase) ||
            client.Contains("mergefin", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        return false;
    }

    private void OnPlaybackStart(object? sender, PlaybackProgressEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Item == null || IsRelayedOrSystemSession(e.Session))
        {
            return;
        }

        _ = Task.Run(() => PropagatePlaybackEventAsync(e, "/Sessions/Playing"));
    }

    private void OnPlaybackStopped(object? sender, PlaybackStopEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Item == null || IsRelayedOrSystemSession(e.Session))
        {
            return;
        }

        _ = Task.Run(() => PropagatePlaybackEventAsync(e, "/Sessions/Playing/Stopped"));
    }

    private void OnUserDataSaved(object? sender, UserDataSaveEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Item == null || e.UserData == null || e.UserId == Guid.Empty)
        {
            return;
        }

        if (e.SaveReason != UserDataSaveReason.TogglePlayed &&
            e.SaveReason != UserDataSaveReason.UpdateUserRating &&
            e.SaveReason != UserDataSaveReason.PlaybackFinished)
        {
            return;
        }

        // Deduplicate rapid repeated events (e.g. from meshed servers) with a 15s cooldown.
        var dedupKey = $"{e.UserId:N}:{e.Item.Id:N}:{e.SaveReason}:{e.UserData.Played}:{e.UserData.IsFavorite}";
        var now = DateTime.UtcNow;
        if (_recentUserData.TryGetValue(dedupKey, out var last) && now - last < UserDataCooldown)
        {
            return;
        }

        _recentUserData[dedupKey] = now;
        _ = Task.Run(() => PropagateUserDataAsync(e));
    }

    private async Task PropagatePlaybackEventAsync(PlaybackProgressEventArgs e, string endpoint)
    {
        try
        {
            var cfg = Plugin.Instance?.Configuration;
            if (cfg == null || e.Item == null || e.Session == null)
            {
                return;
            }

            // Only report live playback to the SINGLE remote server actually serving this stream!
            // If the user is playing a local file on this server, do NOT spawn fake playback cards on remote servers.
            var activeSource = ResolveActiveRemoteSource(e);
            if (activeSource == null)
            {
                return;
            }

            var srv = cfg.Servers.FirstOrDefault(s =>
                s.Enabled && string.Equals(s.Id, activeSource.ServerId, StringComparison.OrdinalIgnoreCase));
            if (srv == null)
            {
                return;
            }

            var localUsername = e.Session.UserName ?? string.Empty;
            var identity = new UpstreamSessionIdentity
            {
                LocalUsername = localUsername,
                ClientName = e.Session.Client ?? "Jellyfin",
                DeviceName = e.Session.DeviceName ?? string.Empty,
                DeviceId = e.Session.DeviceId ?? string.Empty
            };

            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            await _client.ReportPlaystateAsync(
                srv,
                endpoint,
                new
                {
                    ItemId = activeSource.RemoteItemId,
                    MediaSourceId = activeSource.RemoteMediaSourceId,
                    PlaySessionId = e.PlaySessionId ?? string.Empty,
                    PositionTicks = e.PlaybackPositionTicks ?? 0
                },
                identity,
                cts.Token).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin playback event propagation failed");
        }
    }

    private IndexedRemoteSource? ResolveActiveRemoteSource(PlaybackProgressEventArgs e)
    {
        // 1. Check MediaSourceId injected by MergerFinMediaSourceProvider ("mf_<sourceId>").
        if (!string.IsNullOrWhiteSpace(e.MediaSourceId) &&
            e.MediaSourceId.StartsWith("mf_", StringComparison.OrdinalIgnoreCase))
        {
            var src = _store.GetBySourceId(e.MediaSourceId);
            if (src != null)
            {
                return src;
            }
        }

        // 2. Check if the played item is a MergerFin .strm stub pointing to /MergerFin/Stream/<sourceId>.
        var itemPath = e.Item?.Path;
        if (!string.IsNullOrWhiteSpace(itemPath) &&
            itemPath.EndsWith(".strm", StringComparison.OrdinalIgnoreCase) &&
            File.Exists(itemPath))
        {
            try
            {
                var content = File.ReadAllText(itemPath).Trim();
                const string marker = "/MergerFin/Stream/";
                var idx = content.IndexOf(marker, StringComparison.OrdinalIgnoreCase);
                if (idx >= 0)
                {
                    var tail = content[(idx + marker.Length)..];
                    var qIdx = tail.IndexOf('?');
                    var sourceId = qIdx >= 0 ? tail[..qIdx] : tail;
                    var src = _store.GetBySourceId(sourceId);
                    if (src != null)
                    {
                        return src;
                    }
                }
            }
            catch
            {
                // Ignore read errors on .strm
            }
        }

        return null;
    }

    private async Task PropagateUserDataAsync(UserDataSaveEventArgs e)
    {
        try
        {
            var cfg = Plugin.Instance?.Configuration;
            if (cfg == null || e.Item == null || e.UserData == null)
            {
                return;
            }

            var matches = FindRemoteMatchesForItem(e.Item);
            if (matches.Count == 0)
            {
                return;
            }

            var localUserId = e.UserId.ToString("N");
            var localUsername = _userManager.GetUserById(e.UserId)?.Username ?? string.Empty;
            if (string.IsNullOrWhiteSpace(localUsername))
            {
                return;
            }

            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(15));
            foreach (var group in matches.GroupBy(m => m.ServerId))
            {
                var srv = cfg.Servers.FirstOrDefault(s => s.Enabled && string.Equals(s.Id, group.Key, StringComparison.OrdinalIgnoreCase));
                if (srv == null)
                {
                    continue;
                }

                var first = group.First();
                var remoteUserId = await _client.ResolveRemoteUserIdByUsernameAsync(srv, localUsername, cts.Token).ConfigureAwait(false);
                if (string.IsNullOrWhiteSpace(remoteUserId))
                {
                    continue;
                }

                var identity = new UpstreamSessionIdentity
                {
                    LocalUsername = localUsername,
                    ClientName = "Jellyfin",
                    DeviceName = "WatchStateSync",
                    DeviceId = localUserId
                };

                if (e.SaveReason == UserDataSaveReason.TogglePlayed || e.SaveReason == UserDataSaveReason.PlaybackFinished)
                {
                    await _client.MarkPlayedAsync(srv, remoteUserId, first.RemoteItemId, e.UserData.Played, identity, cts.Token).ConfigureAwait(false);
                }

                if (e.SaveReason == UserDataSaveReason.UpdateUserRating)
                {
                    await _client.MarkFavoriteAsync(srv, remoteUserId, first.RemoteItemId, e.UserData.IsFavorite, identity, cts.Token).ConfigureAwait(false);
                }
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin user data propagation failed");
        }
    }

    private List<IndexedRemoteSource> FindRemoteMatchesForItem(BaseItem item)
    {
        if (item is Movie)
        {
            return _store.FindMatches("Movie", item.ProviderIds);
        }

        if (item is Episode ep)
        {
            return _store.FindMatches("Episode", ep.ProviderIds, ep.Series?.ProviderIds, ep.ParentIndexNumber, ep.IndexNumber);
        }

        return [];
    }
}
