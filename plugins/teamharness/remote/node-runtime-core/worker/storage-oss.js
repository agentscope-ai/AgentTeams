#!/usr/bin/env node
"use strict";

const crypto = require("crypto");

function stripEndpoint(endpoint) {
  return String(endpoint || "").trim().replace(/\/+$/, "");
}

function ossPublicEndpointFallback(endpoint) {
  const parsed = new URL(endpoint);
  const suffix = "-internal.aliyuncs.com";
  if (!parsed.hostname.includes("oss-") || !parsed.hostname.endsWith(suffix)) {
    return "";
  }
  parsed.hostname = `${parsed.hostname.slice(0, -suffix.length)}.aliyuncs.com`;
  return parsed.toString().replace(/\/+$/, "");
}

function brokerOssEndpoint(sts) {
  const endpoint = stripEndpoint(sts.oss_endpoint || sts.ossEndpoint || "");
  try {
    return ossPublicEndpointFallback(endpoint) || endpoint;
  } catch {
    return endpoint;
  }
}

function encodeObjectKey(key) {
  return String(key || "")
    .split("/")
    .map(part => encodeURIComponent(part))
    .join("/");
}

function ossRequestUrl(endpoint, bucket, objectKey, query) {
  const parsed = new URL(stripEndpoint(endpoint));
  const pathStyle = parsed.hostname === "localhost" || /^\d+\.\d+\.\d+\.\d+$/.test(parsed.hostname);
  if (pathStyle) {
    parsed.pathname = `/${bucket}${objectKey ? `/${encodeObjectKey(objectKey)}` : "/"}`;
  } else if (parsed.hostname.split(".")[0] === bucket) {
    parsed.pathname = objectKey ? `/${encodeObjectKey(objectKey)}` : "/";
  } else {
    parsed.hostname = `${bucket}.${parsed.hostname}`;
    parsed.pathname = objectKey ? `/${encodeObjectKey(objectKey)}` : "/";
  }
  parsed.search = query || "";
  return parsed;
}

function canonicalizedOssResource(bucket, objectKey, queryKeys) {
  const resource = `/${bucket}${objectKey ? `/${objectKey}` : "/"}`;
  if (!queryKeys || queryKeys.length === 0) {
    return resource;
  }
  return `${resource}?${queryKeys.sort().join("&")}`;
}

function ossAuthHeaders(method, sts, bucket, objectKey, queryKeys) {
  const date = new Date().toUTCString();
  const securityToken = String(sts.security_token || sts.securityToken || "");
  const canonicalHeaders = securityToken ? `x-oss-security-token:${securityToken}\n` : "";
  const canonicalResource = canonicalizedOssResource(bucket, objectKey, queryKeys);
  const stringToSign = `${method}\n\n\n${date}\n${canonicalHeaders}${canonicalResource}`;
  const signature = crypto
    .createHmac("sha1", String(sts.access_key_secret || sts.accessKeySecret || ""))
    .update(stringToSign)
    .digest("base64");
  const headers = {
    Date: date,
    Authorization: `OSS ${String(sts.access_key_id || sts.accessKeyId || "")}:${signature}`
  };
  if (securityToken) {
    headers["x-oss-security-token"] = securityToken;
  }
  return headers;
}

async function ossFetch(method, sts, objectKey, options) {
  const bucket = String(sts.oss_bucket || sts.ossBucket || "").trim();
  const endpoint = stripEndpoint(sts.oss_endpoint || sts.ossEndpoint || "");
  if (!bucket || !endpoint) {
    throw new Error("STS response missing OSS endpoint or bucket");
  }
  const query = options && options.query ? options.query : "";
  const queryKeys = options && options.queryKeys ? options.queryKeys : [];
  const body = options && options.body;
  const tryEndpoint = async candidate => {
    const url = ossRequestUrl(candidate, bucket, objectKey, query);
    const headers = ossAuthHeaders(method, sts, bucket, objectKey, queryKeys);
    const response = await fetch(url, { method, headers, body });
    const data = Buffer.from(await response.arrayBuffer());
    if (!response.ok) {
      throw new Error(`${method} ${url} failed: ${response.status} ${data.toString("utf8")}`);
    }
    return data;
  };
  try {
    return await tryEndpoint(endpoint);
  } catch (error) {
    const fallback = ossPublicEndpointFallback(endpoint);
    if (!fallback) {
      throw error;
    }
    return tryEndpoint(fallback);
  }
}

async function ossGet(sts, objectKey) {
  return (await ossFetch("GET", sts, objectKey)).toString("utf8");
}

async function ossPut(sts, objectKey, content) {
  await ossFetch("PUT", sts, objectKey, { body: Buffer.isBuffer(content) ? content : Buffer.from(String(content)) });
}

async function ossDelete(sts, objectKey) {
  await ossFetch("DELETE", sts, objectKey);
}

async function ossList(sts, prefix) {
  const query = `?prefix=${encodeURIComponent(prefix || "")}`;
  const xml = (await ossFetch("GET", sts, "", { query, queryKeys: [] })).toString("utf8");
  return Array.from(xml.matchAll(/<Key>([^<]+)<\/Key>/g)).map(match => match[1]);
}

module.exports = {
  stripEndpoint,
  ossPublicEndpointFallback,
  brokerOssEndpoint,
  encodeObjectKey,
  ossRequestUrl,
  canonicalizedOssResource,
  ossAuthHeaders,
  ossFetch,
  ossGet,
  ossPut,
  ossDelete,
  ossList
};
