using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Configuration;
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
/// to remote Jellyfin servers with live progress updates, guaranteed stop cleanup,
/// stale-session reaping, and strict loop-prevention for meshed MergerFin setups.
/// </summary>
public class WatchStateSyncService : IHostedService, IDisposable
{
    private static readonly TimeSpan UserDataCooldown = TimeSpan.FromSeconds(15);
    private static readonly TimeSpan ProgressRelayInterval = TimeSpan.FromSeconds(10);
    private static readonly TimeSpan StaleSessionTimeout = TimeSpan.FromSeconds(90);

    private readonly ISessionManager _sessionManager;
    private readonly IUserDataManager _userDataManager;
    private readonly IUserManager _userManager;
    private readonly RemoteCatalogStore _store;
    private readonly UpstreamClient _client;
    private readonly ILogger<WatchStateSyncService> _logger;

    private readonly ConcurrentDictionary<string, DateTime> _recentUserData = new(StringComparer.OrdinalIgnoreCase);
    private readonly ConcurrentDictionary<string, ActiveRemotePlayback> _activeSessions = new(StringComparer.OrdinalIgnoreCase);
    private Timer? _reaperTimer;

    private sealed class ActiveRemotePlayback
    {
        public required RemoteServerConfig Server { get; init; }

        public required IndexedRemoteSource Source { get; init; }

        public required string PlaySessionId { get; init; }

        public required UpstreamSessionIdentity Identity { get; init; }

        public required string LocalUsername { get; init; }

        public long LastPositionTicks { get; set; }

        public bool LastPaused { get; set; }

        public DateTime LastActivityUtc { get; set; }

        public DateTime LastProgressSentUtc { get; set; }
    }

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
        _sessionManager.PlaybackProgress += OnPlaybackProgress;
        _sessionManager.PlaybackStopped += OnPlaybackStopped;
        _userDataManager.UserDataSaved += OnUserDataSaved;
        _reaperTimer = new Timer(ReapStaleSessions, null, TimeSpan.FromSeconds(30), TimeSpan.FromSeconds(30));
        return Task.CompletedTask;
    }

    public Task StopAsync(CancellationToken cancellationToken)
    {
        _sessionManager.PlaybackStart -= OnPlaybackStart;
        _sessionManager.PlaybackProgress -= OnPlaybackProgress;
        _sessionManager.PlaybackStopped -= OnPlaybackStopped;
        _userDataManager.UserDataSaved -= OnUserDataSaved;
        _reaperTimer?.Dispose();
        _reaperTimer = null;

        // Best-effort close any active remote sessions on shutdown.
        foreach (var kv in _activeSessions.ToArray())
        {
            if (_activeSessions.TryRemove(kv.Key, out var active))
            {
                _ = SendStoppedAsync(active, active.LastPositionTicks, false);
            }
        }

        return Task.CompletedTask;
    }

    public void Dispose()
    {
        _reaperTimer?.Dispose();
    }

    private static bool IsRelayedOrSystemSession(SessionInfo? session)
    {
        if (session == null || session.UserId == Guid.Empty)
        {
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

        var activeSource = ResolveActiveRemoteSource(e);
        if (activeSource == null || e.Session == null)
        {
            return;
        }

        var srv = cfg.Servers.FirstOrDefault(s =>
            s.Enabled && string.Equals(s.Id, activeSource.ServerId, StringComparison.OrdinalIgnoreCase));
        if (srv == null)
        {
            return;
        }

        var sessionKey = e.Session.Id;
        var now = DateTime.UtcNow;
        var active = new ActiveRemotePlayback
        {
            Server = srv,
            Source = activeSource,
            PlaySessionId = !string.IsNullOrWhiteSpace(e.PlaySessionId) ? e.PlaySessionId : Guid.NewGuid().ToString("N"),
            LocalUsername = e.Session.UserName ?? string.Empty,
            Identity = new UpstreamSessionIdentity
            {
                LocalUsername = e.Session.UserName ?? string.Empty,
                ClientName = e.Session.Client ?? "Jellyfin",
                DeviceName = e.Session.DeviceName ?? string.Empty,
                DeviceId = e.Session.DeviceId ?? string.Empty
            },
            LastPositionTicks = e.PlaybackPositionTicks ?? 0,
            LastPaused = e.IsPaused,
            LastActivityUtc = now,
            LastProgressSentUtc = now
        };

        // If this local session was previously playing a different remote item, cleanly close the old one first.
        if (_activeSessions.TryGetValue(sessionKey, out var previous) &&
            !string.Equals(previous.PlaySessionId, active.PlaySessionId, StringComparison.OrdinalIgnoreCase))
        {
            _ = Task.Run(() => SendStoppedAsync(previous, previous.LastPositionTicks, false));
        }

        _activeSessions[sessionKey] = active;

        _ = Task.Run(async () =>
        {
            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            await _client.ReportPlaystateAsync(
                active.Server,
                "/Sessions/Playing",
                new
                {
                    ItemId = active.Source.RemoteItemId,
                    MediaSourceId = active.Source.RemoteMediaSourceId,
                    PlaySessionId = active.PlaySessionId,
                    PositionTicks = active.LastPositionTicks,
                    IsPaused = active.LastPaused
                },
                active.Identity,
                cts.Token).ConfigureAwait(false);
        });
    }

    private void OnPlaybackProgress(object? sender, PlaybackProgressEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Session == null || IsRelayedOrSystemSession(e.Session))
        {
            return;
        }

        if (!_activeSessions.TryGetValue(e.Session.Id, out var active))
        {
            return;
        }

        var now = DateTime.UtcNow;
        var prevTicks = active.LastPositionTicks;
        var posTicks = e.PlaybackPositionTicks ?? prevTicks;
        var isPaused = e.IsPaused;
        var pauseChanged = isPaused != active.LastPaused;
        var seeked = Math.Abs(posTicks - prevTicks) > TimeSpan.FromSeconds(15).Ticks;

        active.LastPositionTicks = posTicks;
        active.LastPaused = isPaused;
        active.LastActivityUtc = now;

        if (!pauseChanged && !seeked && now - active.LastProgressSentUtc < ProgressRelayInterval)
        {
            return;
        }

        active.LastProgressSentUtc = now;

        _ = Task.Run(async () =>
        {
            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            await _client.ReportPlaystateAsync(
                active.Server,
                "/Sessions/Playing/Progress",
                new
                {
                    ItemId = active.Source.RemoteItemId,
                    MediaSourceId = active.Source.RemoteMediaSourceId,
                    PlaySessionId = active.PlaySessionId,
                    PositionTicks = posTicks,
                    IsPaused = isPaused,
                    CanSeek = true
                },
                active.Identity,
                cts.Token).ConfigureAwait(false);
        });
    }

    private void OnPlaybackStopped(object? sender, PlaybackStopEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Session == null || IsRelayedOrSystemSession(e.Session))
        {
            return;
        }

        if (!_activeSessions.TryRemove(e.Session.Id, out var active))
        {
            // Fallback in case PlaybackStart was missed (e.g. plugin reloaded mid-playback).
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

            active = new ActiveRemotePlayback
            {
                Server = srv,
                Source = activeSource,
                PlaySessionId = e.PlaySessionId ?? string.Empty,
                LocalUsername = e.Session.UserName ?? string.Empty,
                Identity = new UpstreamSessionIdentity
                {
                    LocalUsername = e.Session.UserName ?? string.Empty,
                    ClientName = e.Session.Client ?? "Jellyfin",
                    DeviceName = e.Session.DeviceName ?? string.Empty,
                    DeviceId = e.Session.DeviceId ?? string.Empty
                },
                LastPositionTicks = e.PlaybackPositionTicks ?? 0,
                LastActivityUtc = DateTime.UtcNow,
                LastProgressSentUtc = DateTime.UtcNow
            };
        }

        var finalPos = e.PlaybackPositionTicks ?? active.LastPositionTicks;
        var runtime = active.Source.RunTimeTicks > 0 ? active.Source.RunTimeTicks : (e.Item?.RunTimeTicks ?? 0);
        var playedToCompletion = e.PlayedToCompletion || (runtime > 0 && finalPos >= (runtime * 9) / 10);

        _ = Task.Run(() => SendStoppedAsync(active, finalPos, playedToCompletion));
    }

    private void ReapStaleSessions(object? state)
    {
        var now = DateTime.UtcNow;
        foreach (var kv in _activeSessions.ToArray())
        {
            if (now - kv.Value.LastActivityUtc > StaleSessionTimeout)
            {
                if (_activeSessions.TryRemove(kv.Key, out var stale))
                {
                    _ = Task.Run(() => SendStoppedAsync(stale, stale.LastPositionTicks, false));
                }
            }
        }
    }

    private async Task SendStoppedAsync(ActiveRemotePlayback active, long positionTicks, bool playedToCompletion)
    {
        try
        {
            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            await _client.ReportPlaystateAsync(
                active.Server,
                "/Sessions/Playing/Stopped",
                new
                {
                    ItemId = active.Source.RemoteItemId,
                    MediaSourceId = active.Source.RemoteMediaSourceId,
                    PlaySessionId = active.PlaySessionId,
                    PositionTicks = positionTicks
                },
                active.Identity,
                cts.Token).ConfigureAwait(false);

            if (!string.IsNullOrWhiteSpace(active.LocalUsername))
            {
                var remoteUserId = await _client.ResolveRemoteUserIdByUsernameAsync(active.Server, active.LocalUsername, cts.Token).ConfigureAwait(false);
                if (!string.IsNullOrWhiteSpace(remoteUserId))
                {
                    await _client.UpdateUserItemDataAsync(
                        active.Server,
                        remoteUserId,
                        active.Source.RemoteItemId,
                        positionTicks,
                        playedToCompletion,
                        active.Identity,
                        cts.Token).ConfigureAwait(false);
                }
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin SendStoppedAsync failed for server {Server}", active.Server.Name);
        }
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

        var dedupKey = $"{e.UserId:N}:{e.Item.Id:N}:{e.SaveReason}:{e.UserData.Played}:{e.UserData.IsFavorite}";
        var now = DateTime.UtcNow;
        if (_recentUserData.TryGetValue(dedupKey, out var last) && now - last < UserDataCooldown)
        {
            return;
        }

        _recentUserData[dedupKey] = now;
        _ = Task.Run(() => PropagateUserDataAsync(e));
    }

    private IndexedRemoteSource? ResolveActiveRemoteSource(PlaybackProgressEventArgs e)
    {
        if (!string.IsNullOrWhiteSpace(e.MediaSourceId) &&
            e.MediaSourceId.StartsWith("mf_", StringComparison.OrdinalIgnoreCase))
        {
            var src = _store.GetBySourceId(e.MediaSourceId);
            if (src != null)
            {
                return src;
            }
        }

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
                    if (e.UserData.Played)
                    {
                        await _client.UpdateUserItemDataAsync(srv, remoteUserId, first.RemoteItemId, 0, true, identity, cts.Token).ConfigureAwait(false);
                    }
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
