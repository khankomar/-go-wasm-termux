const DB_NAME = 'WasmTermuxV86DB';
const DB_VERSION = 1;
const STORE_SNAPSHOTS = 'vm_snapshots';
const PRIMARY_KEY = 'alpine_latest_state';

export class VMStorage {
  constructor() {
    this.dbPromise = this.initDB();
  }

  initDB() {
    return new Promise((resolve, reject) => {
      const request = indexedDB.open(DB_NAME, DB_VERSION);
      request.onupgradeneeded = (event) => {
        const db = event.target.result;
        if (!db.objectStoreNames.contains(STORE_SNAPSHOTS)) {
          db.createObjectStore(STORE_SNAPSHOTS, { keyPath: 'id' });
        }
      };
      request.onsuccess = (event) => resolve(event.target.result);
      request.onerror = (event) => reject(event.target.error);
    });
  }

  async saveSnapshot(arrayBuffer) {
    const db = await this.dbPromise;
    return new Promise((resolve, reject) => {
      const tx = db.transaction([STORE_SNAPSHOTS], 'readwrite');
      const store = tx.objectStore(STORE_SNAPSHOTS);
      const record = {
        id: PRIMARY_KEY,
        buffer: arrayBuffer,
        size: arrayBuffer.byteLength,
        timestamp: Date.now()
      };
      const req = store.put(record);
      req.onsuccess = () => resolve(record);
      req.onerror = (e) => reject(e.target.error);
    });
  }

  async loadSnapshot() {
    const db = await this.dbPromise;
    return new Promise((resolve, reject) => {
      const tx = db.transaction([STORE_SNAPSHOTS], 'readonly');
      const store = tx.objectStore(STORE_SNAPSHOTS);
      const req = store.get(PRIMARY_KEY);
      req.onsuccess = () => resolve(req.result || null);
      req.onerror = (e) => reject(e.target.error);
    });
  }

  async clearSnapshot() {
    const db = await this.dbPromise;
    return new Promise((resolve, reject) => {
      const tx = db.transaction([STORE_SNAPSHOTS], 'readwrite');
      const store = tx.objectStore(STORE_SNAPSHOTS);
      const req = store.delete(PRIMARY_KEY);
      req.onsuccess = () => resolve(true);
      req.onerror = (e) => reject(e.target.error);
    });
  }
}
