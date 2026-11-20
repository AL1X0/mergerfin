using System;
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
/// Listens to local Jellyfin playback & watch-state events and propagates them
/// to matching items on remote Jellyfin servers using per-user mapped accounts or tagged devices.
/// </summary>
public class WatchStateSyncService : IHostedService
{
    private readonly ISessionManager _sessionManager;
    private readonly IUserDataManager _userDataManager;
    private readonly IUserManager _userManager;
    private readonly RemoteCatalogStore _store;
    private readonly UpstreamClient _client;
    private readonly ILogger<WatchStateSyncService> _logger;

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

    private void OnPlaybackStart(object? sender, PlaybackProgressEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Item == null)
        {
            return;
        }

        _ = Task.Run(() => PropagatePlaybackEventAsync(e, "/Sessions/Playing"));
    }

    private void OnPlaybackStopped(object? sender, PlaybackStopEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Item == null)
        {
            return;
        }

        _ = Task.Run(() => PropagatePlaybackEventAsync(e, "/Sessions/Playing/Stopped"));
    }

    private void OnUserDataSaved(object? sender, UserDataSaveEventArgs e)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.SyncWatchStateToUpstreams || e.Item == null || e.UserData == null)
        {
            return;
        }

        if (e.SaveReason != UserDataSaveReason.TogglePlayed &&
            e.SaveReason != UserDataSaveReason.UpdateUserRating &&
            e.SaveReason != UserDataSaveReason.PlaybackFinished)
        {
            return;
        }

        _ = Task.Run(() => PropagateUserDataAsync(e));
    }

    private async Task PropagatePlaybackEventAsync(PlaybackProgressEventArgs e, string endpoint)
    {
        try
        {
            var cfg = Plugin.Instance?.Configuration;
            if (cfg == null || e.Item == null)
            {
                return;
            }

            var matches = FindRemoteMatchesForItem(e.Item);
            if (matches.Count == 0)
            {
                return;
            }

            var localUserId = e.Session?.UserId != Guid.Empty ? e.Session?.UserId.ToString("N") : null;
            var localUsername = e.Session?.UserName ?? string.Empty;

            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(15));
            foreach (var group in matches.GroupBy(m => m.ServerId))
            {
                var srv = cfg.Servers.FirstOrDefault(s => s.Enabled && string.Equals(s.Id, group.Key, StringComparison.OrdinalIgnoreCase));
                if (srv == null)
                {
                    continue;
                }

                var first = group.First();
                var identity = new UpstreamSessionIdentity
                {
                    LocalUsername = localUsername,
                    ClientName = e.Session?.Client ?? "Jellyfin",
                    DeviceName = e.Session?.DeviceName ?? string.Empty,
                    DeviceId = e.Session?.DeviceId ?? string.Empty
                };

                await _client.ReportPlaystateAsync(
                    srv,
                    endpoint,
                    new
                    {
                        ItemId = first.RemoteItemId,
                        MediaSourceId = first.RemoteMediaSourceId,
                        PositionTicks = e.PlaybackPositionTicks ?? 0
                    },
                    identity,
                    cts.Token).ConfigureAwait(false);
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin playback event propagation failed");
        }
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

            var localUserId = e.UserId != Guid.Empty ? e.UserId.ToString("N") : null;
            var localUsername = _userManager.GetUserById(e.UserId)?.Username ?? string.Empty;

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
                    DeviceId = localUserId ?? string.Empty
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

    private System.Collections.Generic.List<IndexedRemoteSource> FindRemoteMatchesForItem(BaseItem item)
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
