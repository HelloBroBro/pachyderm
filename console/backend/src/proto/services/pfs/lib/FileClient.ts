import {ClientWritableStream} from '@grpc/grpc-js';
import {BytesValue} from 'google-protobuf/google/protobuf/wrappers_pb';

import {grpcApiConstructorArgs} from '@dash-backend/proto/utils/createGrpcApiClient';

import {deleteFileFromObject} from '../../../builders/pfs';
import {GRPCPlugin, ServiceArgs} from '../../../lib/types';
import {APIClient} from '../../../proto/pfs/pfs_grpc_pb';
import {AddFile, ModifyFileRequest} from '../../../proto/pfs/pfs_pb';
import {GRPC_MAX_MESSAGE_LENGTH} from '../lib/constants';

export interface FileClientConstructorArgs extends ServiceArgs {
  plugins: GRPCPlugin[];
}

// We need to account for some overhead in the stream data for putFileFromBytes
export const STREAM_OVERHEAD_LENGTH = 17;

let client: APIClient;

export class FileClient<T> {
  protected client: APIClient;
  protected promise: Promise<T>;
  stream: ClientWritableStream<ModifyFileRequest>;

  constructor() {
    this.client =
      client ??
      new APIClient(...grpcApiConstructorArgs(), {
        'grpc.max_receive_message_length': GRPC_MAX_MESSAGE_LENGTH,
        'grpc.max_send_message_length': GRPC_MAX_MESSAGE_LENGTH,
        'grpc-node.max_session_memory': 100,
      });
  }

  putFileFromBytes(
    path: string,
    bytes: Buffer,
    append = false,
    callback?: () => void,
  ) {
    if (!append) this.deleteFile(path);
    const messageLength =
      GRPC_MAX_MESSAGE_LENGTH - STREAM_OVERHEAD_LENGTH - path.length;
    const write = (chunk: Buffer, end: number) => {
      if (chunk.length <= 0) {
        if (callback) callback();
        return;
      } else if (chunk.length > 0) {
        let ok = true;
        const addFile = new AddFile()
          .setPath(path)
          .setRaw(new BytesValue().setValue(chunk));
        ok = this.stream.write(
          new ModifyFileRequest().setAddFile(addFile),
          () => {
            if (ok) {
              write(bytes.slice(end, end + messageLength), end + messageLength);
            } else {
              // We've reached the highWaterMark, wait for all chunks in the buffer to be drained
              this.stream.on('drain', () =>
                write(
                  bytes.slice(end, end + messageLength),
                  end + messageLength,
                ),
              );
            }
          },
        );
      }
    };
    const end = messageLength;
    const chunk = bytes.slice(0, end);
    write(chunk, end);
    return this;
  }

  deleteFile(path: string) {
    this.stream.write(
      new ModifyFileRequest().setDeleteFile(deleteFileFromObject({path})),
    );
    return this;
  }

  end() {
    this.stream.end();
    return this.promise;
  }
}
