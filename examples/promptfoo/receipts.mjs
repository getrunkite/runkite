import { constants } from 'node:fs';
import { link, lstat, mkdir, open, realpath, unlink } from 'node:fs/promises';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

const MAX_RECEIPT_BYTES = 8192;
const RUN_ID = /^pfoo-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/;
// Commas are excluded so a joined duplicate response header cannot be saved.
const REFERENCE = /^[\x21-\x2b\x2d-\x7e]{1,2048}$/;

function privateOwner(info) {
  return (info.mode & 0o077) === 0 && (typeof process.getuid !== 'function' || info.uid === process.getuid());
}

async function syncDirectory(directory) {
  const handle = await open(directory, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
  try { await handle.sync(); } finally { await handle.close(); }
}

async function directoryReady(directory, create = true) {
  if (!path.isAbsolute(directory)) throw new Error('receipt_directory_invalid');
  if (create) {
    try { await mkdir(directory, { mode: 0o700 }); } catch (error) { if (error.code !== 'EEXIST') throw error; }
  }
  const info = await lstat(directory);
  if (!info.isDirectory() || !privateOwner(info) || await realpath(directory) !== directory) {
    throw new Error('receipt_directory_invalid');
  }
  // EEXIST may mean another writer has not yet persisted its mkdir. Every
  // publisher establishes durability rather than relying on that writer.
  if (create) await syncDirectory(path.dirname(directory));
}

// Publish complete, private, fsynced content without replacing another file.
// Unsupported hard links/directory fsync fail before any managed dispatch.
async function publish(directory, destination, value) {
  await directoryReady(directory);
  const data = JSON.stringify(value) + '\n';
  if (Buffer.byteLength(data) > MAX_RECEIPT_BYTES) throw new Error('receipt_too_large');
  const temporary = path.join(directory, `.intent-${randomUUID()}.tmp`);
  let file;
  try {
    file = await open(temporary, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
    await file.writeFile(data, 'utf8');
    await file.sync();
    await file.close();
    file = undefined;
    await link(temporary, destination);
    await syncDirectory(directory);
  } finally {
    await file?.close().catch(() => {});
    await unlink(temporary).catch(() => {});
  }
}

export async function saveIntent(directory, intent) {
  if (!RUN_ID.test(intent.run_id)) throw new Error('receipt_identity_invalid');
  const destination = path.join(directory, `${intent.run_id}.json`);
  await publish(directory, destination, intent);
  return destination;
}

async function readPrivate(filePath) {
  const handle = await open(filePath, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const info = await handle.stat();
    if (!info.isFile() || !privateOwner(info) || info.size > MAX_RECEIPT_BYTES) throw new Error('receipt_invalid');
    const raw = await handle.readFile('utf8');
    if (Buffer.byteLength(raw) > MAX_RECEIPT_BYTES) throw new Error('receipt_invalid');
    return JSON.parse(raw);
  } finally { await handle.close(); }
}

export async function loadIntent(filePath) {
  const absolute = path.resolve(filePath);
  await directoryReady(path.dirname(absolute), false);
  const receipt = await readPrivate(absolute);
  if (!receipt || receipt.version !== 1 || !RUN_ID.test(receipt.run_id) || path.basename(absolute) !== `${receipt.run_id}.json`) {
    throw new Error('receipt_identity_invalid');
  }
  return receipt;
}

function referencePath(receiptPath) { return receiptPath.replace(/\.json$/, '.reference'); }

export async function loadReference(receiptPath) {
  let value;
  try {
    value = await readPrivate(referencePath(receiptPath));
  } catch (error) {
    if (error.code === 'ENOENT') return undefined;
    throw error;
  }
  if (!value || value.version !== 1 || typeof value.reference !== 'string' || !REFERENCE.test(value.reference)) {
    throw new Error('run_reference_invalid');
  }
  // A concurrent publisher may have linked the pin before its directory fsync.
  // Readers also establish durability before using that identity evidence.
  try { await syncDirectory(path.dirname(receiptPath)); }
  catch { throw new Error('run_reference_invalid'); }
  return value.reference;
}

export async function saveReference(receiptPath, reference) {
  if (typeof reference !== 'string' || !REFERENCE.test(reference)) throw new Error('run_reference_invalid');
  try {
    await publish(path.dirname(receiptPath), referencePath(receiptPath), { version: 1, reference });
  } catch (error) {
    if (error.code === 'EEXIST') {
      try {
        if (await loadReference(receiptPath) === reference) {
          // loadReference establishes the matching pin's durability barrier.
          return;
        }
      } catch { /* A failed durability barrier must fail closed below. */ }
    }
    throw new Error('run_reference_invalid');
  }
}
