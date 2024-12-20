# Generated by the protocol buffer compiler.  DO NOT EDIT!
# sources: api/snapshot/snapshot.proto
# plugin: python-betterproto
# This file has been @generated
from dataclasses import dataclass
from datetime import datetime
from typing import (
    TYPE_CHECKING,
    AsyncIterator,
    Dict,
    Iterator,
    Optional,
)

import betterproto
import betterproto.lib.google.protobuf as betterproto_lib_google_protobuf
import grpc


if TYPE_CHECKING:
    import grpc


@dataclass(eq=False, repr=False)
class CreateSnapshotRequest(betterproto.Message):
    metadata: Dict[str, str] = betterproto.map_field(
        1, betterproto.TYPE_STRING, betterproto.TYPE_STRING
    )


@dataclass(eq=False, repr=False)
class CreateSnapshotResponse(betterproto.Message):
    id: int = betterproto.int64_field(1)


@dataclass(eq=False, repr=False)
class DeleteSnapshotRequest(betterproto.Message):
    id: int = betterproto.int64_field(1)


@dataclass(eq=False, repr=False)
class DeleteSnapshotResponse(betterproto.Message):
    pass


@dataclass(eq=False, repr=False)
class SnapshotInfo(betterproto.Message):
    id: int = betterproto.int64_field(1)
    metadata: Dict[str, str] = betterproto.map_field(
        2, betterproto.TYPE_STRING, betterproto.TYPE_STRING
    )
    chunkset_id: int = betterproto.int64_field(3)
    pachyderm_version: str = betterproto.string_field(4)
    created_at: datetime = betterproto.message_field(5)


@dataclass(eq=False, repr=False)
class InspectSnapshotRequest(betterproto.Message):
    id: int = betterproto.int64_field(1)


@dataclass(eq=False, repr=False)
class InspectSnapshotResponse(betterproto.Message):
    info: "SnapshotInfo" = betterproto.message_field(1)
    fileset: str = betterproto.string_field(2)


@dataclass(eq=False, repr=False)
class ListSnapshotRequest(betterproto.Message):
    since: datetime = betterproto.message_field(1)
    limit: int = betterproto.int32_field(2)


@dataclass(eq=False, repr=False)
class ListSnapshotResponse(betterproto.Message):
    info: "SnapshotInfo" = betterproto.message_field(1)


class ApiStub:

    def __init__(self, channel: "grpc.Channel"):
        self.__rpc_create_snapshot = channel.unary_unary(
            "/snapshot.API/CreateSnapshot",
            request_serializer=CreateSnapshotRequest.SerializeToString,
            response_deserializer=CreateSnapshotResponse.FromString,
        )
        self.__rpc_delete_snapshot = channel.unary_unary(
            "/snapshot.API/DeleteSnapshot",
            request_serializer=DeleteSnapshotRequest.SerializeToString,
            response_deserializer=DeleteSnapshotResponse.FromString,
        )
        self.__rpc_inspect_snapshot = channel.unary_unary(
            "/snapshot.API/InspectSnapshot",
            request_serializer=InspectSnapshotRequest.SerializeToString,
            response_deserializer=InspectSnapshotResponse.FromString,
        )
        self.__rpc_list_snapshot = channel.unary_stream(
            "/snapshot.API/ListSnapshot",
            request_serializer=ListSnapshotRequest.SerializeToString,
            response_deserializer=ListSnapshotResponse.FromString,
        )

    def create_snapshot(
        self, *, metadata: Dict[str, str] = None
    ) -> "CreateSnapshotResponse":

        request = CreateSnapshotRequest()
        request.metadata = metadata

        return self.__rpc_create_snapshot(request)

    def delete_snapshot(self, *, id: int = 0) -> "DeleteSnapshotResponse":

        request = DeleteSnapshotRequest()
        request.id = id

        return self.__rpc_delete_snapshot(request)

    def inspect_snapshot(self, *, id: int = 0) -> "InspectSnapshotResponse":

        request = InspectSnapshotRequest()
        request.id = id

        return self.__rpc_inspect_snapshot(request)

    def list_snapshot(
        self, *, since: datetime = None, limit: int = 0
    ) -> Iterator["ListSnapshotResponse"]:

        request = ListSnapshotRequest()
        if since is not None:
            request.since = since
        request.limit = limit

        for response in self.__rpc_list_snapshot(request):
            yield response
