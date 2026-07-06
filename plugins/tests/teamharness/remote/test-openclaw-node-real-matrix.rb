#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "net/http"
require "securerandom"
require "time"
require "uri"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert(condition, message)
  fail!(message) unless condition
end

def env(name)
  ENV[name].to_s.strip
end

MATRIX_URL = env("TEAMHARNESS_MATRIX_URL").sub(%r{/+\z}, "")
WORKER_USER_ID = env("TEAMHARNESS_OPENCLAW_WORKER_USER_ID")
WORKER_NAME = env("TEAMHARNESS_OPENCLAW_WORKER_NAME")
PERSONAL_ROOM_ID = env("TEAMHARNESS_MATRIX_PERSONAL_ROOM_ID")
TEAM_ROOM_ID = env("TEAMHARNESS_MATRIX_TEAM_ROOM_ID")
TIMEOUT_SECONDS = Integer(env("TEAMHARNESS_MATRIX_E2E_TIMEOUT_SECONDS").empty? ? "180" : env("TEAMHARNESS_MATRIX_E2E_TIMEOUT_SECONDS"))

if MATRIX_URL.empty?
  warn "SKIP: TEAMHARNESS_MATRIX_URL is not set; real Matrix trigger smoke not run"
  exit 0
end

if WORKER_USER_ID.empty? && WORKER_NAME.empty?
  warn "SKIP: TEAMHARNESS_OPENCLAW_WORKER_USER_ID or TEAMHARNESS_OPENCLAW_WORKER_NAME is required"
  exit 0
end

if PERSONAL_ROOM_ID.empty? && TEAM_ROOM_ID.empty?
  warn "SKIP: set TEAMHARNESS_MATRIX_PERSONAL_ROOM_ID or TEAMHARNESS_MATRIX_TEAM_ROOM_ID"
  exit 0
end

def matrix_url(path)
  URI("#{MATRIX_URL}#{path}")
end

def http_request(method, path, token: nil, body: nil)
  uri = matrix_url(path)
  request = case method
  when "GET" then Net::HTTP::Get.new(uri)
  when "POST" then Net::HTTP::Post.new(uri)
  when "PUT" then Net::HTTP::Put.new(uri)
  else fail!("unsupported HTTP method: #{method}")
  end
  request["Accept"] = "application/json"
  request["Content-Type"] = "application/json" if body
  request["Authorization"] = "Bearer #{token}" if token && !token.empty?
  request.body = JSON.generate(body) if body

  Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == "https", open_timeout: 10, read_timeout: 35) do |http|
    response = http.request(request)
    payload = response.body.to_s.empty? ? {} : JSON.parse(response.body)
    return [response.code.to_i, payload, response.body.to_s]
  end
end

def matrix_login
  token = env("TEAMHARNESS_MATRIX_TOKEN")
  return token unless token.empty?

  username = env("TEAMHARNESS_MATRIX_USERNAME")
  password = env("TEAMHARNESS_MATRIX_PASSWORD")
  if username.empty? || password.empty?
    warn "SKIP: set TEAMHARNESS_MATRIX_TOKEN or TEAMHARNESS_MATRIX_USERNAME/PASSWORD"
    exit 0
  end

  code, payload, raw = http_request(
    "POST",
    "/_matrix/client/v3/login",
    body: {
      type: "m.login.password",
      identifier: { type: "m.id.user", user: username },
      password: password
    }
  )
  fail!("Matrix login failed: HTTP #{code} #{raw}") unless code == 200
  payload.fetch("access_token")
end

def matrix_whoami(token)
  code, payload, raw = http_request("GET", "/_matrix/client/v3/account/whoami", token: token)
  fail!("Matrix whoami failed: HTTP #{code} #{raw}") unless code == 200
  payload["user_id"].to_s
end

def sync_once(token, since: nil, timeout_ms: 0)
  query = {
    timeout: timeout_ms.to_s,
    filter: JSON.generate(room: { timeline: { limit: 50 } })
  }
  query[:since] = since if since && !since.empty?
  code, payload, raw = http_request("GET", "/_matrix/client/v3/sync?#{URI.encode_www_form(query)}", token: token)
  fail!("Matrix sync failed: HTTP #{code} #{raw}") unless code == 200
  payload
end

def send_room_message(token, room_id, content)
  path = "/_matrix/client/v3/rooms/#{URI.encode_www_form_component(room_id)}/send/m.room.message/#{SecureRandom.hex(8)}"
  code, payload, raw = http_request("PUT", path, token: token, body: content)
  fail!("Matrix send failed for #{room_id}: HTTP #{code} #{raw}") unless code == 200
  payload.fetch("event_id")
end

def event_text(event)
  content = event["content"].is_a?(Hash) ? event["content"] : {}
  new_content = content["m.new_content"].is_a?(Hash) ? content["m.new_content"] : {}
  [new_content["body"], content["body"]].compact.map(&:to_s).join("\n")
end

def wait_for_worker_reply(token, since, room_id, token_text)
  deadline = Time.now + TIMEOUT_SECONDS
  current = since
  loop do
    sync = sync_once(token, since: current, timeout_ms: 30_000)
    current = sync["next_batch"] || current
    joined = sync.dig("rooms", "join") || {}
    events = joined.dig(room_id, "timeline", "events") || []
    events.each do |event|
      next unless event["type"] == "m.room.message"
      next unless event["sender"] == WORKER_USER_ID || (!WORKER_NAME.empty? && event["sender"].to_s.include?(WORKER_NAME))

      text = event_text(event)
      next if text.empty?
      fail!("worker returned OpenClaw failure: #{text}") if text.include?("OpenClaw 执行失败") || text.include?("OpenClaw failed")
      return text if text.include?(token_text)
    end
    fail!("timeout waiting for worker reply containing #{token_text.inspect} in #{room_id}") if Time.now > deadline
  end
end

sender_token = matrix_login
sender_user_id = matrix_whoami(sender_token)
if !WORKER_USER_ID.empty? && sender_user_id == WORKER_USER_ID
  warn "SKIP: Matrix sender #{sender_user_id} is the target worker itself; OpenClaw correctly ignores self messages"
  exit 0
end

initial_sync = sync_once(sender_token)
since = initial_sync.fetch("next_batch")

if !PERSONAL_ROOM_ID.empty?
  token_text = "remote-openclaw-personal-#{SecureRandom.hex(4)}"
  send_room_message(
    sender_token,
    PERSONAL_ROOM_ID,
    {
      msgtype: "m.text",
      body: "请只回复这段测试码，不要添加解释：#{token_text}"
    }
  )
  reply = wait_for_worker_reply(sender_token, since, PERSONAL_ROOM_ID, token_text)
  puts JSON.generate(check: "personal", roomId: PERSONAL_ROOM_ID, reply: reply)
end

if !TEAM_ROOM_ID.empty?
  history_token = "remote-openclaw-history-#{SecureRandom.hex(4)}"
  final_token = "remote-openclaw-team-#{SecureRandom.hex(4)}"
  mention = WORKER_USER_ID.empty? ? "@#{WORKER_NAME}" : WORKER_USER_ID
  send_room_message(
    sender_token,
    TEAM_ROOM_ID,
    {
      msgtype: "m.text",
      body: "这里是一条历史上下文，本次测试码是 #{history_token}。"
    }
  )
  send_room_message(
    sender_token,
    TEAM_ROOM_ID,
    {
      msgtype: "m.text",
      body: "#{mention} 请只回复两个测试码：#{history_token} #{final_token}",
      "m.mentions": WORKER_USER_ID.empty? ? {} : { user_ids: [WORKER_USER_ID] }
    }
  )
  reply = wait_for_worker_reply(sender_token, since, TEAM_ROOM_ID, final_token)
  assert(reply.include?(history_token), "team reply did not include history token #{history_token.inspect}: #{reply}")
  puts JSON.generate(check: "team", roomId: TEAM_ROOM_ID, reply: reply)
end
