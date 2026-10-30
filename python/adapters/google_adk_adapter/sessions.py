"""One-invocation native ADK session service with no second durable store.

This module is imported lazily by the managed runtime so the base runner wheel
does not require the optional Google ADK dependency merely to import or show help.
"""

from __future__ import annotations

import time
from typing import Any

from google.adk.events import Event
from google.adk.sessions import BaseSessionService, Session
from google.adk.sessions.base_session_service import GetSessionConfig, ListSessionsResponse

from .state import dump_session, state_json, validate_session_state


class RunLocalSessionService(BaseSessionService):
    """Implement the public service contract for exactly one trusted session."""

    def __init__(self, *, app_name: str, user_id: str, session_id: str, session: Session | None = None):
        if any(type(item) is not str or not item for item in (app_name, user_id, session_id)):
            raise ValueError("trusted app/user/session identity is required")
        self.app_name, self.user_id, self.session_id = app_name, user_id, session_id
        self._session: Session | None = None
        if session is not None:
            self._check(session.app_name, session.user_id, session.id)
            # Own a public-model copy; do not retain SDK storage revision markers
            # or a caller's mutable session object.
            self._session = Session.model_validate_json(state_json(dump_session(session)))
            validate_session_state(self._session.state, durable=True)

    def _check(self, app_name: str, user_id: str, session_id: str | None = None) -> None:
        if (
            app_name != self.app_name
            or user_id != self.user_id
            or (session_id is not None and session_id != self.session_id)
        ):
            raise ValueError("run-local session service cannot access another app, user or thread")

    async def create_session(
        self, *, app_name: str, user_id: str, state: dict[str, Any] | None = None, session_id: str | None = None
    ) -> Session:
        self._check(app_name, user_id, session_id)
        if self._session is not None:
            raise ValueError("managed native session already exists")
        validate_session_state(state if state is not None else {})
        self._session = Session(
            id=self.session_id,
            app_name=self.app_name,
            user_id=self.user_id,
            state=state if state is not None else {},
            last_update_time=time.time(),
        ).model_copy(deep=True)
        return self._session

    async def get_session(
        self, *, app_name: str, user_id: str, session_id: str, config: GetSessionConfig | None = None
    ) -> Session | None:
        self._check(app_name, user_id, session_id)
        if self._session is None:
            return None
        if config is None or (config.num_recent_events is None and config.after_timestamp is None):
            # This object is private to one managed invocation. Returning the
            # live model lets native ToolContext see invocation-local temp state.
            return self._session
        result = self._session.model_copy(deep=True)
        if config.after_timestamp is not None:
            result.events = [event for event in result.events if event.timestamp >= config.after_timestamp]
        if config.num_recent_events is not None:
            result.events = result.events[-config.num_recent_events :] if config.num_recent_events else []
        return result

    async def list_sessions(self, *, app_name: str, user_id: str | None = None) -> ListSessionsResponse:
        self._check(app_name, user_id if user_id is not None else self.user_id)
        if self._session is None:
            return ListSessionsResponse(sessions=[])
        result = self._session.model_copy(deep=True)
        result.events, result.state = [], {}
        return ListSessionsResponse(sessions=[result])

    async def delete_session(self, *, app_name: str, user_id: str, session_id: str) -> None:
        self._check(app_name, user_id, session_id)
        self._session = None

    async def get_user_state(self, *, app_name: str, user_id: str) -> dict[str, Any]:
        self._check(app_name, user_id)
        raise NotImplementedError("cross-session user state is unsupported by managed Google ADK")

    async def append_event(self, session: Session, event: Event) -> Event:
        self._check(session.app_name, session.user_id, session.id)
        if self._session is None:
            raise ValueError("managed native session does not exist")
        if session is not self._session:
            raise ValueError("cannot append through a detached or filtered native session")
        if event.partial:
            return event
        validate_session_state(session.state)
        validate_session_state(event.actions.state_delta)
        # Native event re-delivery is idempotent only for equal content. Distinct
        # events sharing a provider/test ID remain distinct, matching ADK.
        if any(stored == event for stored in session.events if stored.id == event.id):
            return event
        result = await super().append_event(session=session, event=event)
        session.last_update_time = event.timestamp
        return result

    async def snapshot(self) -> Session:
        if self._session is None:
            raise ValueError("managed native session does not exist")
        result = self._session.model_copy(deep=True)
        result.state = {key: value for key, value in result.state.items() if not key.startswith("temp:")}
        # dump_session checks JSON safety before ADK's permissive serializers.
        # Envelope validation separately proves invocation completion.
        return Session.model_validate_json(state_json(dump_session(result)))
