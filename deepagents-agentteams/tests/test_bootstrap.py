import io

from deepagents_agentteams.bootstrap import fetch_runtime_document


class Response(io.BytesIO):
    def release_conn(self) -> None:
        return None


class FakeMinIO:
    def __init__(self) -> None:
        self.requested = None

    def get_object(self, bucket: str, object_name: str) -> Response:
        self.requested = (bucket, object_name)
        return Response(
            b"apiVersion: agentteams.io/v1beta1\n"
            b"kind: MemberRuntimeConfig\n"
            b"member:\n  name: researcher\n"
        )


def test_fetches_runtime_config_from_runtime_name_prefix() -> None:
    client = FakeMinIO()

    document = fetch_runtime_document(
        {
            "AGENTTEAMS_WORKER_NAME": "researcher",
            "AGENTTEAMS_FS_BUCKET": "agentteams",
        },
        client=client,
    )

    assert client.requested == (
        "agentteams",
        "agents/researcher/runtime/runtime.yaml",
    )
    assert document["kind"] == "MemberRuntimeConfig"
