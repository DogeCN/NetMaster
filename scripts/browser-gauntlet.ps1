# browser-gauntlet.ps1 —— 全链路实测：真实浏览器（headless Edge）经本客户端加载复杂页面。
#
# 为什么必须有这一层：单元测试与 curl 都证明不了"浏览器能不能用"。curl 一次一条
# 连接，而真实页面会并发开几十条子资源 —— 本项目吃过的两次最贵的教训（3 秒探首字节
# 把流杀掉、明文 HTTP 丢 body）都只在浏览器路径上现形。
#
# 用法：
#   pwsh scripts/browser-gauntlet.ps1 -Server proxy.1nf.cc.cd -Password <pw> `
#        -Binary client\netmaster.exe -Runs 2
#
# 判据：每个站点都要拿到非空 <title>（页面真的渲染了），并记录墙钟耗时。
# 任何一站失败即 exit 1 —— 这是验收，不是观测。
param(
  [Parameter(Mandatory = $true)][string]$Server,
  [Parameter(Mandatory = $true)][string]$Password,
  [string]$Binary = "client\netmaster.exe",
  [int]$Runs = 2,
  [int]$Tunnels = 4,
  [string[]]$Sites = @(
    "https://en.wikipedia.org/wiki/Cloudflare",
    "https://www.bbc.com/",
    "https://www.theguardian.com/international",
    "https://news.ycombinator.com/",
    "https://www.cloudflare.com/",
    "https://discord.com/"
  ),
  [string]$Edge = "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe",
  [int]$LoadBudgetSec = 60
)

$ErrorActionPreference = "Stop"
if (-not (Test-Path $Binary)) { throw "client binary not found: $Binary" }
if (-not (Test-Path $Edge)) { throw "Edge not found: $Edge (pass -Edge)" }

function Start-Client($runTag) {
  $log = Join-Path $env:TEMP "gauntlet_$runTag.log"
  Remove-Item $log -ErrorAction SilentlyContinue
  $p = Start-Process -FilePath $Binary -PassThru -NoNewWindow `
    -ArgumentList "serve", "--server", $Server, "--password", $Password, "--manual", "--tunnels", "$Tunnels" `
    -RedirectStandardOutput $log
  # 端口由客户端自己挑（8080 被占会顺延），所以从日志里读真实值，不要假设。
  $deadline = (Get-Date).AddSeconds(30)
  $port = $null
  while ((Get-Date) -lt $deadline) {
    if (Test-Path $log) {
      $m = Select-String -Path $log -Pattern 'HTTP\s+proxy on 127\.0\.0\.1:(\d+)' | Select-Object -First 1
      if ($m) { $port = [int]$m.Matches[0].Groups[1].Value; break }
    }
    Start-Sleep -Milliseconds 100
  }
  if (-not $port) { if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force }; throw "client did not report a proxy port" }
  return @{ Proc = $p; Log = $log; Port = $port }
}

function Wait-FirstSuccess($port, $budgetSec) {
  # 给足超时、只发少数几次：这才是"第一个请求真实花了多久"。
  #
  # 用 5 秒超时轮询会把"5 秒超时"记成一次失败再重试，于是报出来的数字是轮询周期
  # 而不是建连耗时 —— 实测差别很大（真值 ~2s，轮询法报 5.5s）。
  $t0 = Get-Date
  foreach ($attempt in 1..3) {
    $code = & curl.exe -sS -o NUL -w "%{http_code}" --max-time $budgetSec -x "http://127.0.0.1:$port" "https://www.google.com/" 2>$null
    if ($code -eq "200") { return ((Get-Date) - $t0).TotalSeconds }
  }
  return -1
}

$results = @()
foreach ($run in 1..$Runs) {
  $c = Start-Client "run$run"
  try {
    $first = Wait-FirstSuccess $c.Port $LoadBudgetSec
    Write-Output ("=== run {0}: proxy 127.0.0.1:{1}, first-200 after {2:N2}s ===" -f $run, $c.Port, $first)
    foreach ($site in $Sites) {
      $prof = Join-Path $env:TEMP ("edgeprof_" + [guid]::NewGuid().ToString("N").Substring(0, 8))
      $t0 = Get-Date
      $dom = & $Edge --headless=new --disable-gpu --no-first-run --no-default-browser-check `
        "--user-data-dir=$prof" "--proxy-server=http://127.0.0.1:$($c.Port)" --dump-dom $site 2>$null | Out-String
      $secs = ((Get-Date) - $t0).TotalSeconds
      Remove-Item $prof -Recurse -Force -ErrorAction SilentlyContinue
      $title = ""
      if ($dom -match "(?s)<title[^>]*>(.*?)</title>") { $title = ($Matches[1] -replace "\s+", " ").Trim() }
      $ok = $title.Length -gt 0
      $results += [pscustomobject]@{ Run = $run; Site = $site; Secs = [math]::Round($secs, 2); KB = [math]::Round($dom.Length / 1KB); Title = $title; OK = $ok }
      Write-Output ("  {0,-52} {1,6:N2}s  {2,6} KB  {3}  {4}" -f $site, $secs, [math]::Round($dom.Length / 1KB), $(if ($ok) { "OK  " } else { "FAIL" }), $title)
    }
  } finally {
    if (-not $c.Proc.HasExited) { Stop-Process -Id $c.Proc.Id -Force }
    Start-Sleep -Seconds 2
  }
}

$bad = $results | Where-Object { -not $_.OK }
Write-Output ""
Write-Output ("summary: {0}/{1} page loads rendered a title" -f ($results.Count - $bad.Count), $results.Count)
if ($results.Count) {
  $m = ($results | Measure-Object Secs -Average -Maximum)
  Write-Output ("timing: mean {0:N2}s, max {1:N2}s" -f $m.Average, $m.Maximum)
}
if ($bad) {
  $bad | ForEach-Object { Write-Output ("  FAILED: {0} (run {1})" -f $_.Site, $_.Run) }
  exit 1
}
exit 0
